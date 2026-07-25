// Package config holds ODM's option model: the set of all tunables from the PRD
// (§6.2 CLI flags + §7 config-file keys), the `key = value` config-file parser,
// and the merge that implements the §6.3 source priority:
//
//	CLI args  >  ~/.config/odm/config.conf  >  /etc/odm/config.conf  >  defaults
//
// CLI flags override config values only when the flag was explicitly set on the
// command line (`pflag.Flag.Changed`), so a defaulted flag never silently wipes
// a value the user put in their config file.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
)

// Version is the ODM release string; baked into --user-agent default & --version.
const Version = "odm/0.1.0"

// Defaults mirrors the PRD §6.2 default column.
const (
	DefaultConnections = 5
	DefaultMaxConn     = 32
	DefaultMaxRedirect = 5
	DefaultRetry       = 3
	DefaultRetryWait   = 2
	DefaultTimeout     = 30
	DefaultRPCPort     = 6900
	DefaultLogLevel    = "info"
	DefaultConfigPath  = "/etc/odm/config.conf"
	UserConfigRelPath  = "odm/config.conf" // under $XDG_CONFIG_HOME / ~/.config
)

// UserConfigPath resolves ~/.config/odm/config.conf (following XDG_CONFIG_HOME).
func UserConfigPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, UserConfigRelPath), nil
}

// Options is the full set of tunables. Pointer-valued / `*bool`-equivalent
// sentinel handling is described per field; pointers (Args, Output, Dir, etc.)
// are nil when unset so merge decisions stay explicit.
type Options struct {
	// Positional / derived.
	URLs      []string // resolved from positional args + -i input file (post-merge)
	OutFile   string   // --output/-o (single-file only); "" = derive from URL
	Dir       string   // --dir/-d
	InputFile string   // --input-file/-i (path to a URL list)

	// Balancer inputs (§5.1).
	Connections   int // -c
	MaxConnection int // --max-connections  (ceiling; >32 warned)
	SplitFile     int // --split-file/-sf  (0 = unset ⇒ Mode B)

	// Retry / network.
	MaxRedirect int // --max-redirect
	Retry       int // --retry
	RetryWait   int // --retry-wait  (seconds)
	Timeout     int // --timeout      (seconds)

	// HTTP identity / headers.
	UserAgent string   // --user-agent
	Headers   []string // --header/-H (repeatable: "Key: value")
	Referer   string   // --referer
	Proxy     string   // --proxy (http/https/socks5)

	// TLS / integrity.
	CheckCertificate bool   // --check-certificate
	Checksum         string // --checksum "algo:hash"
	LimitRate        string // --limit-rate "5M"/"500K"  ("" = unlimited)
	ChunkSize        string // --chunk-size "4M"

	// Behaviour.
	Yes      bool // --yes/-y
	Quiet    bool // --quiet/-q
	Continue bool // --continue/-x  (resume via .odm control file)

	// Paths / logging.
	ConfigFile string // --config
	LogFile    string // --log
	LogLevel   string // --log-level

	// RPC (§10).
	RPC          bool   // --rpc  (daemon mode)
	RPCPort      int    // --rpc-listen-port
	RPCListenAll bool   // --rpc-listen-all
	RPCSecret    string // --rpc-secret

	// changed tracks which CLI flags were explicitly set, so MergeCLI only
	// applies those. Populated by BindFlags + the FlagSet after parsing.
	changed map[string]bool
}

// Default returns an Options fully initialised from the PRD defaults.
func Default() Options {
	return *DefaultPtr()
}

// DefaultPtr returns a pointer to a freshly defaulted Options; useful for chains
// that need pointer receivers (Load, Apply).
func DefaultPtr() *Options {
	o := &Options{
		Connections:      DefaultConnections,
		MaxConnection:    DefaultMaxConn,
		MaxRedirect:      DefaultMaxRedirect,
		Retry:            DefaultRetry,
		RetryWait:        DefaultRetryWait,
		Timeout:          DefaultTimeout,
		UserAgent:        Version,
		CheckCertificate: true,
		Continue:         true,
		Quiet:            false,
		LogLevel:         DefaultLogLevel,
		ConfigFile:       DefaultConfigPath,
		RPCPort:          DefaultRPCPort,
		ChunkSize:        "4M",
		changed:          map[string]bool{},
	}
	return o
}

// changedFlag records that the named CLI flag was explicitly provided.
func (o *Options) changedFlag(name string) {
	if o.changed == nil {
		o.changed = map[string]bool{}
	}
	o.changed[name] = true
}

// IsSet reports whether the named flag was explicitly set on the CLI.
func (o *Options) IsSet(name string) bool { return o.changed != nil && o.changed[name] }

// --- Config-file parsing (§7) ------------------------------------------------

// ParseFile reads a `key = value` config file, returning the parsed keys. Lines
// starting with `#` (after trimming) and blank lines are ignored. Unknown
// keys are skipped silently (forward-compatible); malformed lines (`foo` with no
// `=`) are returned as an error so users notice typos.
func ParseFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Parse parses config text from an io.Reader (see ParseFile).
func Parse(r io.Reader) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		raw := sc.Text()
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("config %d: invalid line (no '='): %q", lineNo, raw)
		}
		// Inline comments: strip a " #" (space-then-hash) tail that appears
		// after the value. This lets the example config annotate defaults
		// inline (`connections = 5  # budget`). We only strip a `#` that is
		// preceded by whitespace, so a value legitimately containing a bare
		// '#' (rare here — config keys are scalars, URLs come from the CLI)
		// is preserved as long as it isn't " #". A leading '#' (full-line
		// comment) was already handled above.
		out[strings.TrimSpace(key)] = strings.TrimSpace(stripInlineComment(val))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// stripInlineComment removes a trailing " #..." (space, then hash to EOL) from
// a value, preserving any '#' that isn't whitespace-prefixed. Escaping isn't
// supported — values needing a literal trailing " #" are not a supported case
// for ODM's config surface.
func stripInlineComment(val string) string {
	if before, _, ok := strings.Cut(val, " #"); ok {
		return before
	}
	return val
}

// Apply merges parsed config-file keys onto o. Only known keys are applied; an
// unknown key is skipped (forward-compat). A key the user explicitly set on the
// CLI is NOT overwritten — Apply honours §6.3 (CLI > user config > system
// config > defaults) by skipping any key recorded in o.changed (populated by
// CaptureChanged). This is what makes a defaulted CLI flag never wipe a value
// the user put in their config, AND an explicitly-set CLI flag never get
// clobbered by a file value applied afterwards.
//
// Note: o.changed may be nil when Apply is called standalone (outside Setup); in
// that case nothing is marked changed and every file key applies, which is the
// correct behaviour for the pure file-stack test path.
func (o *Options) Apply(kv map[string]string) error {
	for k, v := range kv {
		if o.IsSet(k) {
			// CLI explicitly set this flag; config must not override it.
			continue
		}
		if err := o.setFromKey(k, v); err != nil {
			return fmt.Errorf("config key %q: %w", k, err)
		}
	}
	return nil
}

// setFromKey applies a single key=value to an Options. Keys match the CLI
// long-flag names (without `--`), per PRD §7.
func (o *Options) setFromKey(key, val string) error {
	switch key {
	case "connections":
		return setInt(val, &o.Connections, key)
	case "max-connections":
		return setInt(val, &o.MaxConnection, key)
	case "split-file":
		if val == "" {
			o.SplitFile = 0 // unset ⇒ Mode B
			return nil
		}
		return setInt(val, &o.SplitFile, key)
	case "dir":
		o.Dir = val
	case "output":
		o.OutFile = val
	case "max-redirect":
		return setInt(val, &o.MaxRedirect, key)
	case "retry":
		return setInt(val, &o.Retry, key)
	case "retry-wait":
		return setInt(val, &o.RetryWait, key)
	case "timeout":
		return setInt(val, &o.Timeout, key)
	case "user-agent":
		o.UserAgent = val
	case "referer":
		o.Referer = val
	case "proxy":
		o.Proxy = val
	case "check-certificate":
		return setBool(val, &o.CheckCertificate, key)
	case "continue":
		return setBool(val, &o.Continue, key)
	case "quiet":
		return setBool(val, &o.Quiet, key)
	case "checksum":
		o.Checksum = val
	case "limit-rate":
		o.LimitRate = val
	case "chunk-size":
		o.ChunkSize = val
	case "rpc":
		return setBool(val, &o.RPC, key)
	case "rpc-listen-port":
		return setInt(val, &o.RPCPort, key)
	case "rpc-listen-all":
		return setBool(val, &o.RPCListenAll, key)
	case "rpc-secret":
		o.RPCSecret = val
	case "log":
		o.LogFile = val
	case "log-level":
		o.LogLevel = val
	default:
		// Unknown key: skip (forward-compatible), but ignore safely.
	}
	return nil
}

// --- CLI flags (pflag binding) ----------------------------------------------

// BindFlags wires every §6.2 flag to pointers on o. The returned map lets the
// caller read whether each flag changed after parsing for MergeCLI.
//
// Flags use the long names as their canonical id; short aliases (-c, -sf, -o,
// -d, -i, -y, -q, -x, -H, -V, -h) are added as aliases so both work.
func (o *Options) BindFlags(fs *pflag.FlagSet) {
	fs.IntVarP(&o.Connections, "connections", "c", o.Connections, "Total connection budget")
	fs.IntVar(&o.MaxConnection, "max-connections", o.MaxConnection, "Configurable ceiling for -c/-sf; exceeding it warns")
	// -sf is a 2-char token, which pflag can't bind as a shorthand (shorthands
	// are single ASCII chars and would split -sf into -s -f). We bind the long
	// name only and rewrite bare -sf → --split-file in NormalizeArgs (main.go)
	// so the documented `-sf 4` form keeps working.
	fs.IntVar(&o.SplitFile, "split-file", o.SplitFile, "Parallel connections per file during batch downloads (0 = unset)")
	fs.StringVarP(&o.OutFile, "output", "o", o.OutFile, "Output file name (single-file mode only)")
	fs.StringVarP(&o.Dir, "dir", "d", o.Dir, "Destination directory")
	fs.StringVarP(&o.InputFile, "input-file", "i", o.InputFile, "Read URL list from a file (one URL per line)")

	fs.BoolVarP(&o.Yes, "yes", "y", o.Yes, "Skip the confirmation prompt")
	fs.BoolVarP(&o.Quiet, "quiet", "q", o.Quiet, "Disable the progress bar (for cron/scripts)")
	fs.BoolVarP(&o.Continue, "continue", "x", o.Continue, "Resume an incomplete file (uses the .odm control file)")

	fs.IntVar(&o.MaxRedirect, "max-redirect", o.MaxRedirect, "Max number of redirect hops to follow")
	fs.IntVar(&o.Retry, "retry", o.Retry, "Number of retries per segment on failure")
	fs.IntVar(&o.RetryWait, "retry-wait", o.RetryWait, "Delay between retries (seconds)")
	fs.IntVar(&o.Timeout, "timeout", o.Timeout, "Connection timeout (seconds)")
	fs.StringVar(&o.UserAgent, "user-agent", o.UserAgent, "Custom User-Agent header")
	fs.StringArrayVarP(&o.Headers, "header", "H", o.Headers, "Add a custom HTTP header (repeatable: 'Key: value')")
	fs.StringVar(&o.Referer, "referer", o.Referer, "Set the Referer header")
	fs.StringVar(&o.Proxy, "proxy", o.Proxy, "Proxy (http/https/socks5)")
	fs.BoolVar(&o.CheckCertificate, "check-certificate", o.CheckCertificate, "Verify TLS certificates")
	fs.StringVar(&o.Checksum, "checksum", o.Checksum, "Verify checksum, format algo:hash (md5/sha1/sha256)")
	fs.StringVar(&o.LimitRate, "limit-rate", o.LimitRate, "Global speed limit, e.g. 5M, 500K")
	fs.StringVar(&o.ChunkSize, "chunk-size", o.ChunkSize, "Chunk size for the work-stealing queue, e.g. 4M")
	fs.StringVar(&o.ConfigFile, "config", o.ConfigFile, "Path to a custom config file")
	fs.StringVar(&o.LogFile, "log", o.LogFile, "Log file path")
	fs.StringVar(&o.LogLevel, "log-level", o.LogLevel, "debug / info / warn / error")

	fs.BoolVar(&o.RPC, "rpc", o.RPC, "Run as an RPC server (daemon mode)")
	fs.IntVar(&o.RPCPort, "rpc-listen-port", o.RPCPort, "RPC server port")
	fs.BoolVar(&o.RPCListenAll, "rpc-listen-all", o.RPCListenAll, "Bind to 0.0.0.0 (default 127.0.0.1)")
	fs.StringVar(&o.RPCSecret, "rpc-secret", o.RPCSecret, "RPC authentication token")
}

// NormalizeArgs rewrites the documented 2-char shortcut flags that pflag
// cannot bind as true shorthands (only single-ASCII-char shorthands are
// legal). Practically: `-sf` → `--split-file`. Everything else passes through
// untouched, including bundled single-char shorthands (`-yx`) and `--long`.
//
// It only rewrites a token that is *exactly* `-sf` (or `-sf=val`), so flag
// bundling like `-sf` followed by a value works both as `-sf 4` and `-sf=4`; a
// token like `-sfx` is left alone so it never silently changes meaning.
func NormalizeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch {
		case a == "-sf":
			out = append(out, "--split-file")
		case strings.HasPrefix(a, "-sf="):
			out = append(out, "--split-file="+a[len("-sf="):])
		default:
			out = append(out, a)
		}
	}
	return out
}

// --- Merge & load orchestration -------------------------------------------

// CaptureChanged records which flags of fs were explicitly set, so MergeCLI can
// honour §6.3 priority (CLI overrides config only when the flag was provided).
// Call after fs.Parse().
func (o *Options) CaptureChanged(fs *pflag.FlagSet) {
	o.changed = map[string]bool{}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Changed {
			o.changedFlag(f.Name)
		}
	})
}

// Validate enforces §5.5's basic invariants before any network activity. The
// Balancer does the deeper C/SF/N logic; here we only reject impossible states
// early with a clear message (exit code 1).
func (o *Options) Validate() error {
	if o.Connections < 1 {
		return errors.New("connection budget (-c) must be at least 1")
	}
	if o.SplitFile != 0 && o.SplitFile > o.Connections {
		return errors.New("split-file (-sf) cannot be greater than the total connection budget (-c)")
	}
	if o.RPCPort < 1 || o.RPCPort > 65535 {
		return errors.New("--rpc-listen-port out of range (1-65535)")
	}
	switch o.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid --log-level %q (want debug|info|warn|error)", o.LogLevel)
	}
	if o.Checksum != "" {
		parts := strings.SplitN(o.Checksum, ":", 2)
		if len(parts) != 2 || parts[1] == "" {
			return errors.New("--checksum format is algo:hash (e.g. sha256:<hex>)")
		}
		switch strings.ToLower(parts[0]) {
		case "md5", "sha1", "sha256":
		default:
			return fmt.Errorf("--checksum unsupported algorithm %q (md5/sha1/sha256)", parts[0])
		}
	}
	return nil
}

// --- helpers for setFromKey -------------------------------------------------

func setInt(s string, dst *int, key string) error {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("%s: not an integer (%q)", key, s)
	}
	*dst = v
	return nil
}

func setBool(s string, dst *bool, key string) error {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "1", "on":
		*dst = true
	case "false", "no", "0", "off", "":
		*dst = false
	default:
		return fmt.Errorf("%s: not a boolean (%q)", key, s)
	}
	return nil
}
