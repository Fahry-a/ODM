package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExampleConfigParsesAndValidates guards the shipped configs/odm.conf.example
// : it must parse cleanly with the project's own parser and the
// resulting layered defaults must Validate. This catches a typo'd key, a bad
// default value, or a stray example-only entry drifting out of sync with the
// real key set between releases.
func TestExampleConfigParsesAndValidates(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "odm.conf.example")
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		t.Skipf("example config not found at %s (distribution package only): %v", abs, err)
	}
	kv, err := Parse(strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("example config failed to parse: %v", err)
	}
	o := DefaultPtr()
	if err := o.Apply(kv); err != nil {
		t.Fatalf("example config failed to apply: %v", err)
	}
	if err := o.Validate(); err != nil {
		t.Fatalf("example config produces invalid options: %v", err)
	}
	// Sanity: the documented defaults the example advertises should land
	// where the example says (these are the uncommented value lines).
	if o.Connections != DefaultConnections || o.MaxConnection != DefaultMaxConn {
		t.Fatalf("example defaults drifted: connections=%d max=%d (want %d/%d)",
			o.Connections, o.MaxConnection, DefaultConnections, DefaultMaxConn)
	}
	if !o.Continue {
		t.Fatalf("example should keep continue=true, got %v", o.Continue)
	}
	if !o.CheckCertificate {
		t.Fatalf("example should keep check-certificate=true, got %v", o.CheckCertificate)
	}
	if o.RPCPort != DefaultRPCPort || o.LogLevel != DefaultLogLevel {
		t.Fatalf("example rpc/log defaults drifted: port=%d level=%q", o.RPCPort, o.LogLevel)
	}
	// split-file must be unset (Mode B) per the example's blank value.
	if o.SplitFile != 0 {
		t.Fatalf("example split-file should be unset (0), got %d", o.SplitFile)
	}
}
