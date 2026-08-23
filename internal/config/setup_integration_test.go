package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSetup_BatchURLParsing_Integration exercises the full CLI bootstrap
// (config.Setup) end-to-end for the spec / batch URL parsing cases. The
// existing resolveURLs/NormalizeArgs tests cover those in isolation; this one
// drives the real wired-together path (NormalizeArgs → pflag Parse →
// CaptureChanged → LoadLayers → resolveURLs → Validate) so the layered config
// merge and the URL-resolution heuristic are tested as the binary actually
// runs them.
//
// Hermetic: the system + user config file layers are neutralized so only
// defaults + the CLI flags influence o.URLs. XDG_CONFIG_HOME points at an empty
// temp dir (kills the user-config layer), and --config points at a nonexistent
// path (kills the system-config layer — ParseFile's os.IsNotExist is tolerated
// by LoadLayers). HOME is also pinned so os.UserConfigDir is deterministic.
func TestSetup_BatchURLParsing_Integration(t *testing.T) {
	// Isolate from the host's real ~/.config/odm/config.conf and /etc config.
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("HOME", t.TempDir()) // belt-and-suspenders for os.UserConfigDir

	// A --config path that doesn't exist; LoadLayers tolerates its absence
	// (os.IsNotExist) so only defaults + CLI flags survive.
	missingConfig := filepath.Join(t.TempDir(), "does-not-exist.conf")

	cases := []struct {
		name string
		argv []string
		want []string // expected o.URLs (order-preserving)
	}{
		{
			name: "space-separated positional args (canonical)",
			argv: []string{"-c", "16", "--config", missingConfig,
				"https://files.test.xyz/a.tar.gz", "https://files.test.xyz/b.tar.gz", "https://files.test.xyz/c.tar.gz"},
			want: []string{"https://files.test.xyz/a.tar.gz", "https://files.test.xyz/b.tar.gz", "https://files.test.xyz/c.tar.gz"},
		},
		{
			name: "single comma-containing arg stays ONE URL (legacy split removed)",
			argv: []string{"-c", "16", "--config", missingConfig,
				"https://files.test.xyz/a.tar.gz,https://files.test.xyz/b.tar.gz,https://files.test.xyz/c.tar.gz"},
			want: []string{"https://files.test.xyz/a.tar.gz,https://files.test.xyz/b.tar.gz,https://files.test.xyz/c.tar.gz"},
		},
		{
			name: "URLs with literal commas via space-separated form (comma-safe)",
			argv: []string{"-c", "16", "--config", missingConfig,
				"https://h/x?ids=1,2,3", "https://h/y?ids=4,5,6"},
			want: []string{"https://h/x?ids=1,2,3", "https://h/y?ids=4,5,6"},
		},
		{
			name: "single URL with internal comma is NOT split (comma protected)",
			argv: []string{"-c", "16", "--config", missingConfig,
				"https://h/path?ids=1,2,3"},
			want: []string{"https://h/path?ids=1,2,3"},
		},
		{
			name: "-sf flag rewrites to --split-file and URLs still resolve (Mode C inputs)",
			argv: []string{"-c", "16", "-sf", "4", "--config", missingConfig,
				"https://files.test.xyz/a.tar.gz", "https://files.test.xyz/b.tar.gz"},
			want: []string{"https://files.test.xyz/a.tar.gz", "https://files.test.xyz/b.tar.gz"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o, _, err := Setup(c.argv)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			if got, want := len(o.URLs), len(c.want); got != want {
				t.Fatalf("URL count: got %d want %d (%+v)", got, want, o.URLs)
			}
			for i, w := range c.want {
				if o.URLs[i] != w {
					t.Fatalf("URL[%d]: want %q got %q\nfull: %+v", i, w, o.URLs[i], o.URLs)
				}
			}
		})
	}

	// Separate sub-test: -i input file drives the URLs (positional absent).
	t.Run("-i input file", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "list.txt")
		content := "https://a/file1\n# a comment line\n\n  https://b/file2?ids=1,2\nhttps://c/file3\n"
		if err := os.WriteFile(list, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		o, _, err := Setup([]string{"-c", "8", "-i", list, "--config", missingConfig})
		if err != nil {
			t.Fatalf("Setup -i: %v", err)
		}
		want := []string{"https://a/file1", "https://b/file2?ids=1,2", "https://c/file3"}
		if len(o.URLs) != len(want) {
			t.Fatalf("input file URL count: got %d want %d (%+v)", len(o.URLs), len(want), o.URLs)
		}
		for i, w := range want {
			if o.URLs[i] != w {
				t.Fatalf("input file URL[%d]: want %q got %q", i, w, o.URLs[i])
			}
		}
	})

	// -i combined with positional: both contribute (positional first, then file).
	t.Run("positional plus -i both contribute", func(t *testing.T) {
		dir := t.TempDir()
		list := filepath.Join(dir, "list.txt")
		if err := os.WriteFile(list, []byte("https://fromfile/a\nhttps://fromfile/b\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		o, _, err := Setup([]string{"-c", "4", "-i", list, "--config", missingConfig,
			"https://positional/a"})
		if err != nil {
			t.Fatalf("Setup: %v", err)
		}
		want := []string{"https://positional/a", "https://fromfile/a", "https://fromfile/b"}
		if len(o.URLs) != len(want) {
			t.Fatalf("positional+file URL count: got %d want %d (%+v)", len(o.URLs), len(want), o.URLs)
		}
		for i, w := range want {
			if o.URLs[i] != w {
				t.Fatalf("positional+file URL[%d]: want %q got %q", i, w, o.URLs[i])
			}
		}
	})
}

// TestSetup_LayeredConfigMergedWithURLs confirms Setup honors both the file
// layer and CLI flags simultaneously, then resolves URLs — the integration that
// the isolated Apply/resolveURLs tests can't show on their own. A user --config
// file sets a known connection budget; the CLI repeats -c to override it; both
// must survive onto the returned *Options alongside whatever URLs the CLI gave.
func TestSetup_LayeredConfigMergedWithURLs(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	cfg := filepath.Join(dir, "my.conf")
	// Config sets connections=3; CLI will pass -c 9, which must win.
	if err := os.WriteFile(cfg, []byte("connections = 3\nretry = 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	o, _, err := Setup([]string{"-c", "9", "--config", cfg, "https://files.test.xyz/x.bin"})
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if o.Connections != 9 {
		t.Fatalf("CLI -c must override config connections: got %d", o.Connections)
	}
	if o.Retry != 7 {
		t.Fatalf("ambient config retry must survive when CLI didn't set it: got %d", o.Retry)
	}
	if !o.IsSet("connections") {
		t.Fatalf("connections should be marked explicitly set on the CLI")
	}
	if o.IsSet("retry") {
		t.Fatalf("retry should NOT be marked CLI-set (it came from the config file)")
	}
	if len(o.URLs) != 1 || o.URLs[0] != "https://files.test.xyz/x.bin" {
		t.Fatalf("URL wrong alongside config merge: %+v", o.URLs)
	}
}
