package controlplane

import (
	"fmt"
	"net/http"
	"os"
)

// Build-time metadata used to satisfy AGPL § 13's "offer of corresponding
// source" requirement. These are overridden via -ldflags="-X ..." at build
// time; see the Makefile and scripts/build-release.sh.
//
// Downstream operators running a modified version MUST set SourceURL to the
// publicly reachable location of the actually running source. The /source
// endpoint is unauthenticated by design so that users of the network service
// can always reach it.
var (
	// SourceURL is the canonical, publicly reachable URL of the source code
	// that corresponds to the running binary. Operators of modified builds
	// must repoint this to their own source.
	SourceURL = "https://github.com/agent-testnet/agent-testnet"

	// Version is the semantic version (e.g. "v0.3.1") or "dev".
	Version = "dev"

	// Commit is the short git commit SHA the binary was built from.
	Commit = "unknown"
)

// sourceURL returns the effective source URL, allowing operators to override
// the compiled-in default at runtime via the TESTNET_SOURCE_URL environment
// variable (useful when the same binary is redistributed under a fork).
func sourceURL() string {
	if v := os.Getenv("TESTNET_SOURCE_URL"); v != "" {
		return v
	}
	return SourceURL
}

// handleSource serves the AGPL § 13 "corresponding source" offer. It is
// intentionally unauthenticated and rate-limit-free: every user of the
// network service is entitled to reach the source of the running build.
func (cp *ControlPlane) handleSource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w,
		"Agent Testnet — licensed under AGPL-3.0-or-later.\n"+
			"\n"+
			"This network service is provided under the terms of the GNU Affero\n"+
			"General Public License v3.0 or later. The corresponding source code\n"+
			"for the version running on this server is available at:\n"+
			"\n"+
			"  Source:  %s\n"+
			"  Version: %s\n"+
			"  Commit:  %s\n"+
			"\n"+
			"If you are running a modified version, you are obligated under AGPL\n"+
			"§ 13 to make your modifications available to users of this service.\n"+
			"Set TESTNET_SOURCE_URL or rebuild with -ldflags to point this URL at\n"+
			"your fork.\n",
		sourceURL(), Version, Commit,
	)
}
