package grpcremote

import (
	"os"
	"path/filepath"
	"testing"
)

// Production TLS (AllowServerOnly=false) must reject server-only config: a CA
// alone leaves the signer reachable by any client (PR #300 P1).
func TestTLSRequiresMutualByDefault(t *testing.T) {
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// CA only, mTLS required → error naming the missing client pair.
	if _, err := (&TLSConfig{CAFile: ca}).build(); err == nil {
		t.Fatal("expected error: production TLS must require client cert+key")
	}
	// Explicit dev override accepts server-only (CA present, no client pair).
	// build() may still fail later parsing the dummy CA; we only assert it does
	// NOT fail on the mTLS-requirement check.
	if _, err := (&TLSConfig{CAFile: ca, AllowServerOnly: true}).build(); err != nil {
		if got := err.Error(); got != "" && containsMTLSReq(got) {
			t.Fatalf("AllowServerOnly should bypass the mTLS requirement, got %q", got)
		}
	}
	// New() with a TLS config missing the client pair must fail (no silent
	// server-only production connection).
	if _, err := New(Config{Target: "localhost:9999", TLS: &TLSConfig{CAFile: ca}}); err == nil {
		t.Fatal("New must reject server-only production TLS")
	}
}

func containsMTLSReq(s string) bool {
	for i := 0; i+4 <= len(s); i++ {
		if s[i:i+4] == "mTLS" {
			return true
		}
	}
	return false
}
