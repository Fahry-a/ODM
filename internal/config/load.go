package config

import (
	"bufio"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

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

// systemConfigPath is the layer-2 file. A package var (not the const) so
// tests can point it at a nonexistent path and stay hermetic on hosts that
// really have /etc/odm/config.conf. Production code never reassigns it.
var systemConfigPath = DefaultConfigPath

// LoadLayers applies the file-stack (system → user → explicit --config),
// then resolves positional + -i URLs and validates — all onto the receiver.
// Operating on the receiver is what lets Setup pass the flag-bound *Options
// straight through: pflag wrote the real fields during Parse, so changed flags
// win over the config files layered underneath here.
//
// fs is only consulted for fs.Changed("config"); pass nil when there's no
// FlagSet (the standalone Load path). positional is pflag.Args().
func (o *Options) LoadLayers(fs *pflag.FlagSet, positional []string) error {
	// 2. System config first. The system layer is ALWAYS the default
	// /etc/odm/config.conf — never the --config value: a user passing their
	// own --config must not disable the system layer, and their missing file
	// is reported in step 4, not waved through here.
	sysPath := systemConfigPath
	if kv, err := ParseFile(sysPath); err == nil {
		if applyErr := o.Apply(kv); applyErr != nil {
			return fmt.Errorf("system config %q: %w", sysPath, applyErr)
		}
	} else if !os.IsNotExist(err) {
		// Corrupt/permission-denied system config is worth surfacing.
		return fmt.Errorf("system config %q: %w", sysPath, err)
	}

	// 3. User config overrides system. Missing is fine; a present-but-broken
	// file is surfaced, not silently ignored.
	if up, err := UserConfigPath(); err == nil {
		if kv, perr := ParseFile(up); perr == nil {
			if applyErr := o.Apply(kv); applyErr != nil {
				return fmt.Errorf("user config %q: %w", up, applyErr)
			}
		} else if !os.IsNotExist(perr) {
			return fmt.Errorf("user config %q: %w", up, perr)
		}
	}

	// 4. If the CLI explicitly gave --config, that wins outright — parse that
	// file onto the already-layered base (it can still be overridden by CLI
	// flags, which are already written into *o from Parse). An explicitly
	// named file that cannot be read or applied is an error, not a silent
	// fall-back to defaults (a typo'd path must not drop proxy/secret setup).
	if fs != nil && fs.Changed("config") && o.ConfigFile != sysPath {
		kv, err := ParseFile(o.ConfigFile)
		if err != nil {
			return fmt.Errorf("--config %q: %w", o.ConfigFile, err)
		}
		if applyErr := o.Apply(kv); applyErr != nil {
			return fmt.Errorf("--config %q: %w", o.ConfigFile, applyErr)
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
	if err := o.Validate(); err != nil {
		return err
	}
	// 7. --load-cookies: parse the Netscape cookie file into a Cookie header
	// appended to Headers. Done after validation so a bad file is a clean
	// one-line error; the header rides the existing -H pipeline untouched.
	if o.LoadCookie != "" {
		hdr, err := cookieHeader(o.LoadCookie)
		if err != nil {
			return err
		}
		o.Headers = append(o.Headers, hdr)
	}
	return nil
}

// cookieHeader reads a Netscape-format cookies.txt and returns a
// "Cookie: k=v; k2=v2" header line. Format (tab-separated):
//
//	domain \t flag \t path \t secure \t expiry \t name \t value
//
// with "#HttpOnly_..." prefixed httponly rows and # comments to skip. Only
// the name=value pairs matter for an outbound Cookie header — domain/path/
// secure filtering would need per-request URL knowledge ODM's flat header
// model doesn't have, so all cookies are sent (matching curl's flat-file
// behaviour closely enough for authenticated downloads).
func cookieHeader(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("load-cookies %q: %w", path, err)
	}
	defer f.Close()
	var pairs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "# ") { // real comments start "# "
			continue
		}
		line = strings.TrimPrefix(line, "#HttpOnly_") // httponly marker prefix
		fields := strings.SplitN(line, "\t", 7)
		if len(fields) != 7 {
			continue // malformed row: skip, don't fail the whole file
		}
		name := fields[5]
		// Value is everything after the 6th tab — a cookie value may itself
		// contain tabs; SplitN(…, 7) keeps it whole.
		value := strings.TrimSpace(fields[6])
		if name == "" || value == "" {
			continue
		}
		// Expired cookies are dead weight (and leak stale session ids): skip.
		expiryFields := strings.FieldsFunc(fields[4], func(r rune) bool { return r == ' ' || r == '\t' })
		if len(expiryFields) > 0 {
			if expiry, err := strconv.ParseInt(expiryFields[0], 10, 64); err == nil && expiry > 0 && time.Now().Unix() >= expiry {
				continue
			}
		}
		pairs = append(pairs, name+"="+value)
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("load-cookies %q: %w", path, err)
	}
	if len(pairs) == 0 {
		return "", fmt.Errorf("load-cookies %q: no cookies found", path)
	}
	return "Cookie: " + strings.Join(pairs, "; "), nil
}

// resolveURLs implements batch URL parsing:
//   - positional args present → each arg is one URL (comma-safe).
//   - -i file → appended (one URL per line, # comments + blanks skipped).
//   - none → error (caller decides: --rpc allows empty).
func resolveURLs(o *Options, positional []string) ([]string, error) {
	var out []string

	if len(positional) > 0 {
		// Each positional arg is exactly one URL; commas inside an arg are
		// literal query/fragment content and never separators.
		out = make([]string, 0, len(positional))
		for _, a := range positional {
			if a != "" {
				out = append(out, a)
			}
		}
	}

	// -i input file: one URL per line, or a Metalink4 (.meta4/.metalink)
	// document carrying mirror URLs + a verification hash per file.
	if o.InputFile != "" && isMetalinkName(o.InputFile) {
		return parseMetalink(o.InputFile, o)
	}
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

// isMetalinkName reports whether path looks like a Metalink4 document.
func isMetalinkName(path string) bool {
	return strings.HasSuffix(strings.ToLower(path), ".meta4") ||
		strings.HasSuffix(strings.ToLower(path), ".metalink")
}

// readMetalink caps the document read: metalinks carry URLs and hashes, never
// bulk data, so 16 MiB is generous — an unbounded ReadFile would let a hostile
// .meta4 exhaust memory.
func readMetalink(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 16<<20))
}

// parseMetalink reads a Metalink4 XML file (RFC 5854) and registers its first
// <file> entry: every mirror URL becomes a download target (primary first),
// and the entry's <hash> — when present — is stored as the --checksum spec so
// verification happens automatically. Later entries are ignored for now: ODM's
// batch model is flat URLs, and multi-file metalinks map awkwardly onto the
// one-checksum-per-run rule.
func parseMetalink(path string, o *Options) ([]string, error) {
	data, err := readMetalink(path)
	if err != nil {
		return nil, fmt.Errorf("metalink %q: %w", path, err)
	}
	var doc struct {
		Files []struct {
			Name   string `xml:"name"`
			Size   int64  `xml:"size"`
			Hashes []struct {
				Type string `xml:"type,attr"`
				Val  string `xml:",chardata"`
			} `xml:"hash"`
			URLs []struct {
				Location string `xml:"location,attr"`
				Priority string `xml:"priority,attr"`
				Val      string `xml:",chardata"`
			} `xml:"url"`
		} `xml:"file"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("metalink %q: %w", path, err)
	}
	if len(doc.Files) == 0 {
		return nil, fmt.Errorf("metalink %q: no <file> entries", path)
	}
	f0 := doc.Files[0]
	// RFC 5854: priority=1 is the highest; a missing priority sinks the URL
	// to the end. Stable so equal priorities keep document order.
	type prioURL struct {
		val  string
		prio int
	}
	ps := make([]prioURL, 0, len(f0.URLs))
	for _, u := range f0.URLs {
		v := strings.TrimSpace(u.Val)
		if v == "" {
			continue
		}
		p := math.MaxInt32
		if n, err := strconv.Atoi(strings.TrimSpace(u.Priority)); err == nil && n >= 1 {
			p = n
		}
		ps = append(ps, prioURL{val: v, prio: p})
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].prio < ps[j].prio })
	urls := make([]string, 0, len(ps))
	for _, p := range ps {
		urls = append(urls, p.val)
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("metalink %q: file %q has no URLs", path, f0.Name)
	}
	// First URL = primary download source; the rest feed --mirror.
	pick := ""
	for _, h := range f0.Hashes {
		t := strings.ToLower(strings.TrimSpace(h.Type))
		switch t {
		case "sha256":
			pick = t + ":" + strings.ToLower(strings.TrimSpace(h.Val))
		case "sha1":
			if pick == "" || strings.HasPrefix(pick, "md5:") {
				pick = t + ":" + strings.ToLower(strings.TrimSpace(h.Val))
			}
		case "md5":
			if pick == "" {
				pick = t + ":" + strings.ToLower(strings.TrimSpace(h.Val))
			}
		}
	}
	if pick != "" && o.Checksum == "" {
		o.Checksum = pick
	}
	o.Mirrors = append(o.Mirrors, urls[1:]...)
	// ONE task: the primary URL only. Returning every mirror as its own batch
	// target used to spawn same-basename tasks writing the SAME destination
	// concurrently (silent WriteAt overlap corruption), and the >1-URL batch
	// rule then discarded o.Checksum — losing metalink verification entirely.
	return urls[:1], nil
}
