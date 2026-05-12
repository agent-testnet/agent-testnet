package cli

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/spf13/cobra"
)

func newCertsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "certs",
		Short: "Fetch TLS certificates from the testnet control plane",
	}

	cmd.AddCommand(newCertsFetchCmd())
	return cmd
}

func newCertsFetchCmd() *cobra.Command {
	var (
		name   string
		secret string
		outDir string
	)

	cmd := &cobra.Command{
		Use:   "fetch",
		Short: "Fetch certificates and write cert.pem, key.pem, ca.pem to disk",
		Long: `Fetches TLS certificates from the testnet control plane and writes them
to the output directory. The certificate includes SANs for all domains
declared for this node in nodes.yaml.

Point your reverse proxy (nginx, Caddy) at these files for TLS termination.`,
		Example: `  # Fetch certs with flags
  testnet-toolkit certs fetch \
    --server-url https://203.0.113.10:8443 \
    --name forum --secret shared-secret --out-dir /etc/testnet/certs

  # Fetch certs with environment variables
  SERVER_URL=https://203.0.113.10:8443 NODE_NAME=forum NODE_SECRET=s3cret \
    testnet-toolkit certs fetch`,
		RunE: func(cmd *cobra.Command, args []string) error {
			url := resolveServerURL(cmd)

			if name == "" {
				name = os.Getenv("NODE_NAME")
			}
			if secret == "" {
				secret = os.Getenv("NODE_SECRET")
			}
			if name == "" || secret == "" {
				return fmt.Errorf("--name/NODE_NAME and --secret/NODE_SECRET are required")
			}
			if outDir == "" {
				if v := os.Getenv("CERT_OUT_DIR"); v != "" {
					outDir = v
				} else {
					outDir = "/etc/testnet/certs"
				}
			}

			log.Printf("Fetching TLS certificates from %s for node %q...", url, name)
			client := api.NewServerClient(url, nil)
			certs, err := client.FetchNodeCerts(name, secret)
			if err != nil {
				return fmt.Errorf("fetch certs: %w", err)
			}

			if err := os.MkdirAll(outDir, 0o755); err != nil {
				return fmt.Errorf("create output directory: %w", err)
			}

			files := []struct {
				name string
				data string
				perm os.FileMode
			}{
				{"cert.pem", certs.CertPEM, 0o600},
				{"key.pem", certs.KeyPEM, 0o600},
				{"ca.pem", certs.CAPEM, 0o644},
			}

			for _, f := range files {
				path := filepath.Join(outDir, f.name)
				if err := os.WriteFile(path, []byte(f.data), f.perm); err != nil {
					return fmt.Errorf("write %s: %w", f.name, err)
				}
				log.Printf("  wrote %s (%d bytes)", path, len(f.data))
			}

			log.Println("Done. Configure your reverse proxy to use these certificates.")
			return nil
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "node name from nodes.yaml [$NODE_NAME]")
	cmd.Flags().StringVar(&secret, "secret", "", "per-node secret from nodes.yaml [$NODE_SECRET]")
	cmd.Flags().StringVar(&outDir, "out-dir", "", "directory to write cert files (default /etc/testnet/certs) [$CERT_OUT_DIR]")

	return cmd
}
