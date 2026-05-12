package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/agent-testnet/agent-testnet/client/daemon"
	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
)

func newAgentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage agent VMs",
	}

	cmd.AddCommand(newAgentLaunchCmd())
	cmd.AddCommand(newAgentStopCmd())
	cmd.AddCommand(newAgentListCmd())

	return cmd
}

func newAgentLaunchCmd() *cobra.Command {
	var (
		rootfs     string
		vcpu       int
		memMB      int
		standalone bool
	)

	cmd := &cobra.Command{
		Use:   "launch",
		Short: "Launch a new agent VM",
		RunE: func(cmd *cobra.Command, args []string) error {
			agentCfg := api.AgentConfig{
				RootFS: rootfs,
				VCPU:   vcpu,
				MemMB:  memMB,
			}

			if standalone {
				return launchStandalone(agentCfg)
			}

			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-launch",
				Payload: agentCfg,
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable (try --standalone): %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("launch failed: %s", resp.Error)
			}

			data, _ := json.MarshalIndent(resp.Payload, "", "  ")
			fmt.Printf("Agent launched:\n%s\n", string(data))
			return nil
		},
	}

	cmd.Flags().StringVar(&rootfs, "rootfs", "", "path to rootfs image")
	cmd.Flags().IntVar(&vcpu, "vcpu", 0, "number of vCPUs (default: from config)")
	cmd.Flags().IntVar(&memMB, "mem", 0, "memory in MB (default: from config)")
	cmd.Flags().BoolVar(&standalone, "standalone", true, "launch without daemon (default for MVP)")

	return cmd
}

func launchStandalone(agentCfg api.AgentConfig) error {
	home, _ := os.UserHomeDir()

	var clientCfg *config.ClientConfig
	if cfg != nil {
		clientCfg = cfg
	} else {
		configPath := home + "/.testnet/config.yaml"
		var err error
		clientCfg, err = config.LoadClientConfig(configPath)
		if err != nil {
			return fmt.Errorf("load config (%s): %w", configPath, err)
		}
	}

	d, err := daemon.New(clientCfg)
	if err != nil {
		return fmt.Errorf("init: %w", err)
	}

	agent, err := d.LaunchAgent(agentCfg)
	if err != nil {
		return fmt.Errorf("launch agent: %w", err)
	}

	info := agent.Info()
	fmt.Printf("Agent launched!\n")
	fmt.Printf("  ID:       %s\n", info.ID)
	fmt.Printf("  Guest IP: %s\n", info.TunnelIP)
	fmt.Printf("  vCPU:     %d\n", info.VCPU)
	fmt.Printf("  Memory:   %d MB\n", info.MemMB)
	fmt.Printf("  Log:      %s\n", agent.LogPath())
	if keyPath := agent.SSHKeyPath(); keyPath != "" {
		fmt.Printf("  SSH:      ssh -i %s root@%s\n", keyPath, info.TunnelIP)
	}
	fmt.Println("\nVM is running. Press Ctrl+C to stop.")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		<-sigCh
		fmt.Println("\nStopping agent...")
		cancel()
	}()

	<-ctx.Done()
	if err := d.StopAgent(info.ID); err != nil {
		fmt.Printf("Warning: stop error: %v\n", err)
	}
	fmt.Println("Agent stopped.")
	return nil
}

func newAgentStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop [agent-id]",
		Short: "Stop a running agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-stop",
				Payload: map[string]string{"id": args[0]},
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("stop failed: %s", resp.Error)
			}
			fmt.Printf("Agent %s stopped.\n", args[0])
			return nil
		},
	}
}

func newAgentListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List running agents",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-list",
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("list failed: %s", resp.Error)
			}

			data, _ := json.MarshalIndent(resp.Payload, "", "  ")
			fmt.Printf("Agents:\n%s\n", string(data))
			return nil
		},
	}
}
