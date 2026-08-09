package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestParseCommentsAndBlankLines(t *testing.T) {
	r := strings.NewReader(`
# a comment
connections = 8

  # indented comment
dir = /tmp/odm
`)
	kv, err := Parse(r)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if kv["connections"] != "8" || kv["dir"] != "/tmp/odm" {
		t.Fatalf("kv = %+v", kv)
	}
	if _, bad := kv["# a comment"]; bad {
		t.Fatalf("comment leaked into kv")
	}
}

func TestParseMalformedLine(t *testing.T) {
	if _, err := Parse(strings.NewReader("no equals here")); err == nil {
		t.Fatalf("want error for line without '='")
	}
}

func TestParseInlineComments(t *testing.T) {
	r := strings.NewReader(`
connections = 5          # total budget
max-redirect = 5          # redirect hops to follow
dir = /tmp/odm            # destination
limit-rate = 5M           # global cap; '#' legit in values? rare here
piece = odd#hash          # bare hash no space → preserved
`)
	kv, err := Parse(r)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if kv["connections"] != "5" {
		t.Fatalf("inline comment not stripped for connections: %q", kv["connections"])
	}
	if kv["max-redirect"] != "5" {
		t.Fatalf("max-redirect value %q", kv["max-redirect"])
	}
	if kv["dir"] != "/tmp/odm" {
		t.Fatalf("dir value %q", kv["dir"])
	}
	if kv["limit-rate"] != "5M" {
		t.Fatalf("limit-rate value %q", kv["limit-rate"])
	}
	// A '#' with no leading space is part of the value, not a comment.
	if kv["piece"] != "odd#hash" {
		t.Fatalf("bare hash should be preserved, got %q", kv["piece"])
	}
}

func TestApplyKnownKeys(t *testing.T) {
	o := DefaultPtr()
	kv := map[string]string{
		"connections":       "12",
		"max-connections":   "64",
		"split-file":        "4",
		"dir":               "/d",
		"check-certificate": "false",
		"continue":          "false",
		"quiet":             "true",
		"limit-rate":        "1M",
		"log-level":         "debug",
		"rpc":               "true",
		"rpc-listen-port":   "9999",
		"rpc-listen-all":    "true",
		"rpc-secret":        "s3cr3t",
		"rpc-tls-cert":      "/etc/odm/odm.crt",
		"rpc-tls-key":       "/etc/odm/odm.key",
	}
	if err := o.Apply(kv); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if o.Connections != 12 || o.MaxConnection != 64 || o.SplitFile != 4 || o.Dir != "/d" {
		t.Fatalf("int/dir fields wrong: %+v", o)
	}
	if o.CheckCertificate || o.Continue || !o.Quiet || !o.RPC || !o.RPCListenAll {
		t.Fatalf("bool fields wrong: %+v", o)
	}
	if o.LimitRate != "1M" || o.LogLevel != "debug" || o.RPCSecret != "s3cr3t" || o.RPCPort != 9999 {
		t.Fatalf("str/port wrong: %+v", o)
	}
	if o.RPCTLSCert != "/etc/odm/odm.crt" || o.RPCTLSKey != "/etc/odm/odm.key" {
		t.Fatalf("tls fields wrong: cert=%q key=%q", o.RPCTLSCert, o.RPCTLSKey)
	}
}

func TestApplySplitFileEmptyUnsets(t *testing.T) {
	o := DefaultPtr()
	o.SplitFile = 5
	if err := o.Apply(map[string]string{"split-file": ""}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if o.SplitFile != 0 {
		t.Fatalf("empty split-file should unset (Mode B), got %d", o.SplitFile)
	}
}

func TestApplyInvalidInt(t *testing.T) {
	o := DefaultPtr()
	if err := o.Apply(map[string]string{"connections": "abc"}); err == nil {
		t.Fatalf("want error for non-int connections")
	}
}

func TestApplyInvalidBool(t *testing.T) {
	o := DefaultPtr()
	if err := o.Apply(map[string]string{"quiet": "maybe"}); err == nil {
		t.Fatalf("want error for non-bool quiet")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		mut  func(o *Options) *Options
		ok   bool
	}{
		{"defaults", func(o *Options) *Options { return o }, true},
		{"c=0", func(o *Options) *Options { o.Connections = 0; return o }, false},
		{"sf>c", func(o *Options) *Options { o.Connections = 4; o.SplitFile = 9; return o }, false},
		{"bad port", func(o *Options) *Options { o.RPCPort = 0; return o }, false},
		{"bad loglevel", func(o *Options) *Options { o.LogLevel = "verbose"; return o }, false},
		{"bad checksum fmt", func(o *Options) *Options { o.Checksum = "sha256"; return o }, false},
		{"bad checksum algo", func(o *Options) *Options { o.Checksum = "crc32:deadbeef"; return o }, false},
		{"good checksum", func(o *Options) *Options { o.Checksum = "sha256:" + strings.Repeat("a", 64); return o }, true},
		{"tls both unset", func(o *Options) *Options { return o }, true},
		{"tls both set", func(o *Options) *Options { o.RPCTLSCert = "/a/b.crt"; o.RPCTLSKey = "/a/b.key"; return o }, true},
		{"tls cert only", func(o *Options) *Options { o.RPCTLSCert = "/a/b.crt"; return o }, false},
		{"tls key only", func(o *Options) *Options { o.RPCTLSKey = "/a/b.key"; return o }, false},
		// Engine profile validation.
		{"profile aria2c", func(o *Options) *Options { o.Profile = "aria2c"; return o }, true},
		{"profile both", func(o *Options) *Options { o.Profile = "both"; return o }, true},
		{"profile smart", func(o *Options) *Options { o.Profile = "smart"; return o }, true},
		{"profile bogus", func(o *Options) *Options { o.Profile = "banana"; return o }, false},
		{"sf + aria2c", func(o *Options) *Options { o.Profile = "aria2c"; o.SplitFile = 4; return o }, false},
		{"chunk-size + both", func(o *Options) *Options {
			o.Profile = "both"
			o.changedFlag("chunk-size")
			return o
		}, false},
		{"split + odm", func(o *Options) *Options {
			o.changedFlag("split")
			o.Split = 4
			return o
		}, false},
		{"split + aria2c ok", func(o *Options) *Options { o.Profile = "aria2c"; o.Split = 4; return o }, true},
		{"max-conn-per-server 0", func(o *Options) *Options { o.Profile = "aria2c"; o.MaxConnPerServer = 0; return o }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.mut(DefaultPtr()).Validate()
			if (err == nil) != c.ok {
				t.Fatalf("ok=%v but err=%v", c.ok, err)
			}
		})
	}
}

func TestResolveURLs_SpaceSeparated(t *testing.T) {
	o := DefaultPtr()
	urls, err := resolveURLs(o, []string{"https://a/x?a=1,2", "https://b/y", "https://c/z"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(urls) != 3 || urls[0] != "https://a/x?a=1,2" {
		t.Fatalf("space-separated must keep commas inside URLs: %+v", urls)
	}
}

func TestResolveURLs_LegacyComma(t *testing.T) {
	o := DefaultPtr()
	urls, err := resolveURLs(o, []string{"https://a/x,https://b/y,https://c/z"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(urls) != 3 {
		t.Fatalf("want 3 urls from legacy comma form, got %d (%+v)", len(urls), urls)
	}
}

func TestResolveURLs_SingleURLWithCommaNotSplit(t *testing.T) {
	// A single positional arg that looks like one URL (scheme before first
	// comma) must NOT be split, even though it contains a comma.
	o := DefaultPtr()
	urls, err := resolveURLs(o, []string{"https://h/path?ids=1,2,3"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(urls) != 1 {
		t.Fatalf("single URL with comma must not split, got %+v", urls)
	}
}

func TestResolveURLs_InputFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "list.txt")
	content := "https://a\n# comment\n\nhttps://b\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	o := DefaultPtr()
	o.InputFile = p
	urls, err := resolveURLs(o, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(urls) != 2 || urls[0] != "https://a" || urls[1] != "https://b" {
		t.Fatalf("input file parse wrong: %+v", urls)
	}
}

// TestApply_UserOverridesSystem exercises the layered file-stack merge without
// spinning a real argc; it tests Apply precedence (user > system) directly,
// which is what Load composes on top of the CLI-flag Changed logic (Apply skips
// any key the CLI explicitly set, so pflag-written flags win by construction).
func TestApply_UserOverridesSystem(t *testing.T) {
	sys := map[string]string{"connections": "4", "dir": "/sys"}
	user := map[string]string{"connections": "20"}
	o := DefaultPtr()
	_ = o.Apply(sys)  // system first
	_ = o.Apply(user) // user second
	if o.Connections != 20 {
		t.Fatalf("user must override system: got %d", o.Connections)
	}
	if o.Dir != "/sys" { // untouched by user → keep system
		t.Fatalf("ambient system value lost: dir=%q", o.Dir)
	}
}

func TestNormalizeArgs_SF(t *testing.T) {
	in := []string{"-c", "16", "-sf", "4", "https://x/y", "-sf=7"}
	out := NormalizeArgs(in)
	want := []string{"-c", "16", "--split-file", "4", "https://x/y", "--split-file=7"}
	if len(out) != len(want) {
		t.Fatalf("len mismatch: %v vs %v", out, want)
	}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("arg[%d]: want %q got %q", i, want[i], out[i])
		}
	}
}

// Identical-shaped bundling like "-sf" must not turn into "--split-file" if it
// is part of a longer bundled token — keep behavior predictable.
func TestNormalizeArgs_DoesNotMangleBundled(t *testing.T) {
	out := NormalizeArgs([]string{"-sfx"})
	if out[0] != "-sfx" {
		t.Fatalf("bundled -sfx must pass through, got %q", out[0])
	}
}

func TestCaptureChangedAndIsSet(t *testing.T) {
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	o := DefaultPtr()
	o.BindFlags(fs)
	_ = fs.Parse(NormalizeArgs([]string{"-c", "9", "-sf", "3"}))
	o.CaptureChanged(fs)
	if !o.IsSet("connections") {
		t.Fatalf("connections should be marked changed")
	}
	if o.IsSet("quiet") {
		t.Fatalf("quiet should NOT be changed")
	}
	if !o.IsSet("split-file") {
		t.Fatalf("split-file (via -sf) should be marked changed")
	}
}
