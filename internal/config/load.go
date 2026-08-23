package config

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/pflag"
)

// Setup is the single CLI bootstrap wired into cmd/odm/main.go. It owns the
// ordering subtlety that Load alone cannot enforce: pflag must bind its
// pointer targets to the *same* *Options that the file-stack + URL resolution
// later mutate, otherwise the values pflag writes during Parse land on a
// throwaway struct and the CLI's flags are silently dropped. Setup keeps one
// Options alive across:
//
//  1. BindFlags      — flag pointers wired into `o`
//  2. Parse          — pflag fills `o` and records which flags Changed
//  3. CaptureChanged — snapshot Changed into o.changed
//  4. LoadLayers     — layer /etc → ~/.config → --config onto the same `o`
//  5. resolveURLs + Validate
//
// Returns the populated *Options and the FlagSet (main needs both for the
// early -V/--help handles and pflag.Args()). Parse errors and validation
// errors propagate as-is to the caller.
func Setup(argv []string) (*Options, *pflag.FlagSet, error) {
	args := NormalizeArgs(argv)
	o := DefaultPtr()
	fs := pflag.NewFlagSet("odm", pflag.ContinueOnError)
	fs.SetOutput(io.Discard) // main renders errors + help itself
	o.BindFlags(fs)
	if err := fs.Parse(args); err != nil {
		return nil, fs, err
	}
	o.CaptureChanged(fs)
	positional := fs.Args()
	if err := o.LoadLayers(fs, positional); err != nil {
		return nil, fs, err
	}
	return o, fs, nil
}

// Load layers the config priority onto a freshly-defaulted *Options built
// internally, given a FlagSet whose only need is fs.Changed("config"). It is the
// standalone entry used by tests that don't want to run a full Parse. Production
// CLI code uses Setup instead: Setup binds flag pointers to the live *Options so
// pflag-written values survive, whereas Load's internal DefaultPtr() means any
// flag-bound value would be thrown away — so do NOT wire the CLI through Load.
//
// argsAfterFlags is the leftover positional argv slice (pflag.Args()).
func Load(fs *pflag.FlagSet, argsAfterFlags []string) (*Options, error) {
	o := DefaultPtr()
	if err := o.LoadLayers(fs, argsAfterFlags); err != nil {
		return nil, err
	}
	return o, nil
}

// LoadLayers applies the file-stack (system → user → explicit --config),
// then resolves positional + -i URLs and validates — all onto the receiver.
// Operating on the receiver is what lets Setup pass the flag-bound *Options
// straight through: pflag wrote the real fields during Parse, so changed flags
// win over the config files layered underneath here.
//
// fs is only consulted for fs.Changed("config"); pass nil when there's no
// FlagSet (the standalone Load path). positional is pflag.Args().
func (o *Options) LoadLayers(fs *pflag.FlagSet, positional []string) error {
	// 2. System config first. Save the original path so a later CLI --config
	// override is detectable (the BindFlags default already equals this).
	sysPath := o.ConfigFile
	if kv, err := ParseFile(sysPath); err == nil {
		_ = o.Apply(kv)
	} else if !os.IsNotExist(err) {
		// Corrupt/permission-denied system config is worth surfacing.
		return fmt.Errorf("system config %q: %w", sysPath, err)
	}

	// 3. User config overrides system. Missing is fine.
	if up, err := UserConfigPath(); err == nil {
		if kv, perr := ParseFile(up); perr == nil {
			_ = o.Apply(kv)
		}
	}

	// 4. If the CLI explicitly gave --config, that wins outright — parse that
	// file onto the already-layered base (it can still be overridden by CLI
	// flags, which are already written into *o from Parse).
	if fs != nil && fs.Changed("config") && o.ConfigFile != sysPath {
		if kv, err := ParseFile(o.ConfigFile); err == nil {
			_ = o.Apply(kv)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("--config %q: %w", o.ConfigFile, err)
		}
	}

	// 5. There is deliberately no "merge CLI" step here — CLI flags win by
	// CONSTRUCTION, not by post-hoc merging. pflag already wrote every
	// explicitly-given flag straight into this *Options during Parse (see
	// BindFlags), and the Apply calls above skip any key recorded in o.changed
	// (CaptureChanged). So the file layers applied in steps 2-4 — system →
	// user → --config, later wins for unchanged keys — layer on top of a
	// CLI-filled struct and can never clobber a flag the user actually typed.

	// 6. Resolve positional URLs + -i input file, then validate.
	urls, err := resolveURLs(o, positional)
	if err != nil {
		return err
	}
	o.URLs = urls
	return o.Validate()
}

// resolveURLs implements batch URL parsing:
//   - positional args present → each arg is one URL (canonical, comma-safe).
//   - exactly one positional arg containing commas → legacy comma-delimited list.
//   - -i file → appended (one URL per line, # comments + blanks skipped).
//   - none → error (caller decides: --rpc allows empty).
func resolveURLs(o *Options, positional []string) ([]string, error) {
	var out []string

	switch {
	case len(positional) > 1:
		// Canonical space-separated form: each arg is one URL.
		out = make([]string, 0, len(positional))
		for _, a := range positional {
			if a != "" {
				out = append(out, a)
			}
		}
	case len(positional) == 1:
		// Legacy comma-delimited form ONLY when the single arg contains a
		// comma that is *followed by* another scheme prefix — that is the one
		// reliable signal that the comma separates two URLs rather than living
		// inside one URL's query/fragment. A bare "https://h?ids=1,2" has no
		// scheme after the comma, so it stays a single URL (comma-safe).
		if commaFollowedByScheme(positional[0]) {
			// Legacy comma-delimited single-arg form.
			for p := range strings.SplitSeq(positional[0], ",") {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
		} else {
			out = []string{positional[0]}
		}
	}

	// -i input file: one URL per line.
	if o.InputFile != "" {
		f, err := os.Open(o.InputFile)
		if err != nil {
			return nil, fmt.Errorf("input file %q: %w", o.InputFile, err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("input file %q: %w", o.InputFile, err)
		}
	}

	return out, nil
}

// commaFollowedByScheme reports whether s contains a comma that is followed by
// an http:// or https:// scheme prefix. This is the heuristic separator for
// the legacy comma-delimited single-arg batch form: "https://a/x,https://b/y"
// splits, but "https://h?ids=1,2,3" (comma inside a single URL) does not, since
// nothing after its comma looks like a new URL. See spec — this form is
// intentionally approximate which is why space-separated is recommended.
func commaFollowedByScheme(s string) bool {
	rest := s
	for {
		i := strings.IndexByte(rest, ',')
		if i < 0 {
			return false
		}
		after := strings.TrimSpace(rest[i+1:])
		if strings.HasPrefix(after, "http://") || strings.HasPrefix(after, "https://") {
			return true
		}
		rest = rest[i+1:]
	}
}
