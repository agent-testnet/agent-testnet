// Package sslenv is the single source of truth for the SSL/TLS environment
// variables that testnet sandboxes inject so every common runtime trusts the
// testnet CA without any per-tool configuration.
//
// Background: when a testnet sandbox (the Firecracker agent VM, or the
// toolkit's network-namespace sandbox for node-side processes) installs the
// testnet CA via update-ca-certificates, the system trust store
// (/etc/ssl/certs/ca-certificates.crt) starts trusting it. That fixes
// OpenSSL-based tools — curl, wget, Python's stdlib ssl, Ruby, Perl, Go,
// libcurl-linked applications — but NOT the major runtimes that ship their
// own bundled CA list:
//
//   - Node.js (undici/fetch) — bundled CAs only; honors NODE_EXTRA_CA_CERTS.
//   - Python `requests`/`urllib3` — certifi bundle; honors REQUESTS_CA_BUNDLE.
//   - AWS SDKs (boto3, aws-sdk-js, ...) — own bundle; honors AWS_CA_BUNDLE.
//   - git over HTTPS — own config; honors GIT_SSL_CAINFO.
//
// Setting these env vars system-wide inside the sandbox is the most universal
// fix short of MITM proxying. It is additive — NODE_EXTRA_CA_CERTS extends
// Node's bundled CAs rather than replacing them, so calls to real-internet
// services (LLM APIs, package registries reached via the install-time proxy)
// still validate against public CAs.
package sslenv

import (
	"fmt"
	"strings"
)

// Standard locations populated by ca-certificates + update-ca-certificates on
// Alpine and Debian-family distros. Both are part of our base rootfs (see
// scripts/gen-rootfs.sh) and the network-namespace sandbox's target hosts.
const (
	// SystemBundlePath is the combined PEM bundle produced by
	// update-ca-certificates. Honored by OpenSSL-aware env vars.
	SystemBundlePath = "/etc/ssl/certs/ca-certificates.crt"

	// SystemCertsDir is the OpenSSL hashed-cert directory.
	SystemCertsDir = "/etc/ssl/certs"

	// TestnetCAPath is the raw testnet root CA PEM. We point
	// NODE_EXTRA_CA_CERTS here (rather than at SystemBundlePath) so Node
	// keeps trusting its bundled public CAs and only ADDS the testnet CA.
	// See client/sandbox/firecracker.go and toolkit/sandbox/namespace.go
	// for where the cert is written.
	TestnetCAPath = "/usr/local/share/ca-certificates/testnet/testnet-ca.crt"
)

// Pairs returns the canonical (name, value) list of env vars to inject. This
// is the single source of truth; all other helpers in this package derive
// from it.
func Pairs() [][2]string {
	return [][2]string{
		{"SSL_CERT_FILE", SystemBundlePath},
		{"SSL_CERT_DIR", SystemCertsDir},
		{"CURL_CA_BUNDLE", SystemBundlePath},
		{"REQUESTS_CA_BUNDLE", SystemBundlePath},
		{"AWS_CA_BUNDLE", SystemBundlePath},
		{"GIT_SSL_CAINFO", SystemBundlePath},
		{"NODE_EXTRA_CA_CERTS", TestnetCAPath},
	}
}

// EnvSlice returns the env vars formatted as "KEY=VALUE" strings suitable for
// exec.Cmd.Env. Use for per-process injection (e.g. the network-namespace
// sandbox where we can't modify the shared host filesystem).
func EnvSlice() []string {
	pairs := Pairs()
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p[0] + "=" + p[1]
	}
	return out
}

// Shell returns the env vars as a POSIX shell snippet of `export KEY="VALUE"`
// lines, suitable for /etc/profile.d/*.sh. Values are double-quoted; none of
// the values we emit contain double-quotes, dollar signs, or backticks, so no
// escaping is required and we keep the output readable.
func Shell() string {
	var b strings.Builder
	for _, p := range Pairs() {
		fmt.Fprintf(&b, "export %s=\"%s\"\n", p[0], p[1])
	}
	return b.String()
}

// EnvironmentFile returns the env vars as plain "KEY=VALUE" lines, suitable
// for /etc/environment (read by pam_env at login). Same format as EnvSlice
// but joined with newlines and trailing newline.
func EnvironmentFile() string {
	var b strings.Builder
	for _, p := range Pairs() {
		fmt.Fprintf(&b, "%s=%s\n", p[0], p[1])
	}
	return b.String()
}
