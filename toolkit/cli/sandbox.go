package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/agent-testnet/agent-testnet/toolkit/sandbox"
	"github.com/spf13/cobra"
)

func newSandboxCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sandbox",
		Short: "Run a process confined to the testnet network",
	}

	cmd.AddCommand(newSandboxRunCmd())
	return cmd
}

func newSandboxRunCmd() *cobra.Command {
	var (
		dnsIP       string
		caCert      string
		wgInterface string
		allowedCIDR string
	)

	cmd := &cobra.Command{
		Use:   "run [flags] -- <command> [args...]",
		Short: "Execute a command inside a testnet-confined network namespace",
		Long: `Creates a Linux network namespace where the child process can only reach
testnet services. DNS is pointed at the testnet DNS server, the testnet CA
is installed in the namespace's trust store, and all other outbound traffic
is dropped.

Requires root or CAP_NET_ADMIN + CAP_SYS_ADMIN.`,
		Example: `  # Run a crawler confined to the testnet
  testnet-toolkit sandbox run \
    --dns-ip 83.150.0.1 \
    --ca-cert /etc/testnet/certs/ca.pem \
    -- /usr/local/bin/my-crawler --seeds /var/lib/seeds.txt

  # Run wget inside the sandbox
  testnet-toolkit sandbox run -- wget --mirror https://reddit.com/`,
		DisableFlagParsing: false,
		Args:               cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Geteuid() != 0 {
				return fmt.Errorf("sandbox run requires root privileges")
			}

			if dnsIP == "" {
				if v := os.Getenv("DNS_IP"); v != "" {
					dnsIP = v
				} else {
					dnsIP = "83.150.0.1"
				}
			}
			if caCert == "" {
				if v := os.Getenv("CA_CERT_PATH"); v != "" {
					caCert = v
				} else {
					caCert = "/etc/testnet/certs/ca.pem"
				}
			}
			if wgInterface == "" {
				if v := os.Getenv("WG_INTERFACE"); v != "" {
					wgInterface = v
				} else {
					wgInterface = "wg0"
				}
			}

			cidrs := strings.Split(allowedCIDR, ",")
			for i := range cidrs {
				cidrs[i] = strings.TrimSpace(cidrs[i])
			}

			cfg := &sandbox.Config{
				DNSIP:        dnsIP,
				CACertPath:   caCert,
				WGInterface:  wgInterface,
				AllowedCIDRs: cidrs,
				Command:      args[0],
				Args:         args[1:],
			}

			return sandbox.Run(cfg)
		},
	}

	cmd.Flags().StringVar(&dnsIP, "dns-ip", "", "testnet DNS address (default 83.150.0.1) [$DNS_IP]")
	cmd.Flags().StringVar(&caCert, "ca-cert", "", "path to testnet CA cert (default /etc/testnet/certs/ca.pem) [$CA_CERT_PATH]")
	cmd.Flags().StringVar(&wgInterface, "wg-interface", "", "WireGuard interface to route through (default wg0) [$WG_INTERFACE]")
	cmd.Flags().StringVar(&allowedCIDR, "allowed-cidrs", "83.150.0.0/16,10.99.0.0/16", "comma-separated CIDRs reachable from the sandbox [$ALLOWED_CIDRS]")

	return cmd
}
