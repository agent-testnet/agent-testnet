package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var serverURL string

func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "testnet-toolkit",
		Short: "Composable tools for integrating services with the agent testnet",
		Long: `testnet-toolkit provides three tool groups for integrating existing applications
with the agent testnet:

  certs    Fetch TLS certificates from the testnet control plane
  seed     Discover testnet domains and output seed URLs
  sandbox  Run a process confined to the testnet network`,
	}

	root.PersistentFlags().StringVar(&serverURL, "server-url", "", "control plane URL (e.g. https://203.0.113.10:8443) [$SERVER_URL]")

	root.AddCommand(newCertsCmd())
	root.AddCommand(newSeedCmd())
	root.AddCommand(newSandboxCmd())

	return root
}

func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func resolveServerURL(cmd *cobra.Command) string {
	if serverURL != "" {
		return serverURL
	}
	if v := os.Getenv("SERVER_URL"); v != "" {
		return v
	}
	cmd.PrintErrln("Error: --server-url or $SERVER_URL is required")
	os.Exit(1)
	return ""
}
