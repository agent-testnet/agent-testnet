package sslenv

import (
	"strings"
	"testing"
)

func TestPairsContainsExpectedVars(t *testing.T) {
	required := []string{
		"SSL_CERT_FILE",
		"SSL_CERT_DIR",
		"CURL_CA_BUNDLE",
		"REQUESTS_CA_BUNDLE",
		"AWS_CA_BUNDLE",
		"GIT_SSL_CAINFO",
		"NODE_EXTRA_CA_CERTS",
	}

	got := make(map[string]string)
	for _, p := range Pairs() {
		got[p[0]] = p[1]
	}

	for _, name := range required {
		val, ok := got[name]
		if !ok {
			t.Errorf("Pairs() missing %q", name)
			continue
		}
		if val == "" {
			t.Errorf("Pairs()[%q] is empty", name)
		}
	}
}

func TestNodeExtraCACertsPointsToRawPEM(t *testing.T) {
	// Node's NODE_EXTRA_CA_CERTS is ADDITIVE to its bundled CA list; pointing
	// it at the system bundle would still work but is wasteful and confusing.
	// Lock in that it points at the raw testnet CA file.
	for _, p := range Pairs() {
		if p[0] == "NODE_EXTRA_CA_CERTS" {
			if p[1] != TestnetCAPath {
				t.Errorf("NODE_EXTRA_CA_CERTS = %q, want %q (raw testnet PEM, not the system bundle)", p[1], TestnetCAPath)
			}
			return
		}
	}
	t.Fatal("NODE_EXTRA_CA_CERTS not present in Pairs()")
}

func TestEnvSliceFormat(t *testing.T) {
	out := EnvSlice()
	if len(out) != len(Pairs()) {
		t.Fatalf("EnvSlice length %d, want %d", len(out), len(Pairs()))
	}
	for _, line := range out {
		if !strings.Contains(line, "=") {
			t.Errorf("EnvSlice entry %q missing '='", line)
		}
		if strings.HasPrefix(line, "=") || strings.HasSuffix(line, "=") {
			t.Errorf("EnvSlice entry %q has empty key or value", line)
		}
	}
}

func TestShellFormat(t *testing.T) {
	out := Shell()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(Pairs()) {
		t.Fatalf("Shell line count %d, want %d", len(lines), len(Pairs()))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "export ") {
			t.Errorf("Shell line %q missing `export ` prefix", line)
		}
		if !strings.Contains(line, "=\"") || !strings.HasSuffix(line, "\"") {
			t.Errorf("Shell line %q not in `export KEY=\"VALUE\"` form", line)
		}
	}
}

func TestEnvironmentFileFormat(t *testing.T) {
	out := EnvironmentFile()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != len(Pairs()) {
		t.Fatalf("EnvironmentFile line count %d, want %d", len(lines), len(Pairs()))
	}
	for _, line := range lines {
		if strings.HasPrefix(line, "export ") {
			t.Errorf("EnvironmentFile line %q has `export ` (PAM expects raw KEY=VALUE)", line)
		}
		if !strings.Contains(line, "=") {
			t.Errorf("EnvironmentFile line %q missing '='", line)
		}
	}
}
