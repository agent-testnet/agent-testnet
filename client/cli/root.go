package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/agent-testnet/agent-testnet/pkg/config"
)

var (
	cfgFile string
	cfg     *config.ClientConfig
)

// NewRootCmd creates the root Cobra command for testnet-client.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "testnet-client",
		Short: "Testnet agent sandbox client",
		Long:  "Manages Firecracker agent VMs connected to the testnet via WireGuard tunnel.",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if cmd.Name() == "setup" || cmd.Name() == "install" {
				return nil
			}

			var err error
			if cfgFile != "" {
				cfg, err = config.LoadClientConfig(cfgFile)
			} else {
			home, _ := os.UserHomeDir()
			cfg, err = config.LoadClientConfig(home + "/.testnet/config.yaml")
			if err != nil {
				cfg = &config.ClientConfig{}
				config.SetClientDefaults(cfg)
				return nil
			}
			}
			return err
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", "", "config file (default: ~/.testnet/config.yaml)")

	root.AddCommand(newInstallCmd())
	root.AddCommand(newSetupCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newAgentCmd())

	return root
}

// Execute runs the CLI.
func Execute() {
	if err := NewRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
