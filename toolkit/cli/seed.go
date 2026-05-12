package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/spf13/cobra"
)

var (
	seedAPIToken    string
	seedExcludeNode string
)

func newSeedCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Discover testnet domains and output seed URLs",
		Long: `Queries the control plane for all registered domains and outputs them
in various formats. Use this to feed crawlers, link checkers, or any
tool that needs to know what exists on the testnet.`,
	}

	cmd.PersistentFlags().StringVar(&seedAPIToken, "api-token", "", "API token for authenticated control plane calls [$API_TOKEN]")
	cmd.PersistentFlags().StringVar(&seedExcludeNode, "exclude-node", "", "node name to exclude from output (e.g. self) [$EXCLUDE_NODE]")

	cmd.AddCommand(newSeedURLsCmd())
	cmd.AddCommand(newSeedDomainsCmd())
	cmd.AddCommand(newSeedJSONCmd())

	return cmd
}

func fetchAndFilterDomains(cmd *cobra.Command) ([]api.DomainMapping, error) {
	url := resolveServerURL(cmd)

	if seedAPIToken == "" {
		seedAPIToken = os.Getenv("API_TOKEN")
	}
	if seedExcludeNode == "" {
		if v := os.Getenv("EXCLUDE_NODE"); v != "" {
			seedExcludeNode = v
		}
	}

	client := api.NewServerClient(url, nil)
	client.APIToken = seedAPIToken

	domains, err := client.ListDomains()
	if err != nil {
		return nil, fmt.Errorf("list domains: %w", err)
	}

	if seedExcludeNode == "" {
		return domains, nil
	}

	filtered := make([]api.DomainMapping, 0, len(domains))
	for _, d := range domains {
		if d.Node != seedExcludeNode {
			filtered = append(filtered, d)
		}
	}
	return filtered, nil
}

func newSeedURLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "urls",
		Short: "Output https://{domain}/ for each testnet domain, one per line",
		Example: `  testnet-toolkit seed urls --server-url https://203.0.113.10:8443 --api-token <token>
  testnet-toolkit seed urls --exclude-node search > /var/lib/crawler/seeds.txt`,
		RunE: func(cmd *cobra.Command, args []string) error {
			domains, err := fetchAndFilterDomains(cmd)
			if err != nil {
				return err
			}
			for _, d := range domains {
				fmt.Printf("https://%s/\n", d.Domain)
			}
			return nil
		},
	}
}

func newSeedDomainsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "domains",
		Short: "Output raw domain names, one per line",
		Example: `  testnet-toolkit seed domains --server-url https://203.0.113.10:8443 --api-token <token>`,
		RunE: func(cmd *cobra.Command, args []string) error {
			domains, err := fetchAndFilterDomains(cmd)
			if err != nil {
				return err
			}
			for _, d := range domains {
				fmt.Println(d.Domain)
			}
			return nil
		},
	}
}

func newSeedJSONCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "json",
		Short: "Output the full domain list as JSON",
		Example: `  testnet-toolkit seed json --server-url https://203.0.113.10:8443 --api-token <token> | jq .`,
		RunE: func(cmd *cobra.Command, args []string) error {
			domains, err := fetchAndFilterDomains(cmd)
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(domains)
		},
	}
}
