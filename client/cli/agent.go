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
	cmd.AddCommand(newAgentProxyCmd())

	return cmd
}

func newAgentLaunchCmd() *cobra.Command {
	var (
		rootfs     string
		vcpu       int
		memMB      int
		standalone bool
		jsonOut    bool
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
				return launchStandalone(agentCfg, jsonOut)
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

			return printPayload(resp.Payload, jsonOut, "Agent launched")
		},
	}

	cmd.Flags().StringVar(&rootfs, "rootfs", "", "path to rootfs image")
	cmd.Flags().IntVar(&vcpu, "vcpu", 0, "number of vCPUs (default: from config)")
	cmd.Flags().IntVar(&memMB, "mem", 0, "memory in MB (default: from config)")
	cmd.Flags().BoolVar(&standalone, "standalone", true, "launch without daemon (default for MVP)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print only the JSON payload (machine-readable)")

	return cmd
}

func launchStandalone(agentCfg api.AgentConfig, jsonOut bool) error {
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
	if jsonOut {
		data, _ := json.MarshalIndent(info, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("Agent launched!\n")
		fmt.Printf("  ID:       %s\n", info.ID)
		fmt.Printf("  Guest IP: %s\n", info.TunnelIP)
		fmt.Printf("  vCPU:     %d\n", info.VCPU)
		fmt.Printf("  Memory:   %d MB\n", info.MemMB)
		fmt.Printf("  Log:      %s\n", agent.LogPath())
		if info.SSHKeyPath != "" {
			fmt.Printf("  SSH:      ssh -i %s root@%s\n", info.SSHKeyPath, info.TunnelIP)
		}
		fmt.Println("\nVM is running. Press Ctrl+C to stop.")
	}

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
	var jsonOut bool
	cmd := &cobra.Command{
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
			return printPayload(resp.Payload, jsonOut, "Agents")
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print only the JSON payload (machine-readable)")
	return cmd
}

// ----- agent proxy subcommands -----

func newAgentProxyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Manage per-VM passthrough proxies (e.g. expose api.openrouter.ai to the agent)",
		Long: "Per-VM passthrough proxies let an agent VM reach a real upstream\n" +
			"domain through a host-side TCP forwarder. The forwarder is bound to a\n" +
			"public-looking IP (83.150.255.x by default) or a private per-VM IP\n" +
			"(172.16.<vm>.x). Public is the default — useful for agents whose URL\n" +
			"fetch SSRF guard rejects RFC1918 / 'special-use' addresses (e.g.\n" +
			"OpenClaw blocking https://openrouter.ai when it resolves to 172.16/12).",
	}
	cmd.AddCommand(newAgentProxyAddCmd())
	cmd.AddCommand(newAgentProxyRemoveCmd())
	cmd.AddCommand(newAgentProxyListCmd())
	return cmd
}

func newAgentProxyAddCmd() *cobra.Command {
	var (
		visibility string
		ports      []int
		upstream   string
		jsonOut    bool
	)

	cmd := &cobra.Command{
		Use:   "add <agent-id> <domain>",
		Short: "Add a passthrough proxy for an agent VM",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			payload := api.ProxyConfig{
				AgentID:    args[0],
				Domain:     args[1],
				Visibility: visibility,
				Ports:      ports,
				Upstream:   upstream,
			}
			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-proxy-add",
				Payload: payload,
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("proxy add failed: %s", resp.Error)
			}
			return printPayload(resp.Payload, jsonOut, "Proxy added")
		},
	}

	cmd.Flags().StringVar(&visibility, "visibility", api.ProxyVisibilityPublic,
		"IP visibility: public (83.150.255.x, default) or private (172.16.<vm>.x)")
	cmd.Flags().IntSliceVar(&ports, "port", []int{443},
		"TCP port(s) to forward (repeatable, e.g. --port 80 --port 443)")
	cmd.Flags().StringVar(&upstream, "upstream", "",
		"Upstream HOST:PORT (default: <domain>:<first port>)")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print only the JSON payload (machine-readable)")

	return cmd
}

func newAgentProxyRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <agent-id> <domain>",
		Short: "Stop a passthrough proxy for an agent VM",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-proxy-remove",
				Payload: api.ProxyRef{AgentID: args[0], Domain: args[1]},
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("proxy remove failed: %s", resp.Error)
			}
			fmt.Printf("Proxy %s removed from %s.\n", args[1], args[0])
			return nil
		},
	}
}

func newAgentProxyListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list <agent-id>",
		Short: "List passthrough proxies for an agent VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{
				Command: "agent-proxy-list",
				Payload: api.ProxyRef{AgentID: args[0]},
			})
			if err != nil {
				return fmt.Errorf("daemon unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("proxy list failed: %s", resp.Error)
			}
			return printPayload(resp.Payload, jsonOut, fmt.Sprintf("Proxies for %s", args[0]))
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print only the JSON payload (machine-readable)")
	return cmd
}

// printPayload prints a daemon response payload either as a header + indented
// JSON (default, for human use) or just the raw JSON (--json, for scripts).
func printPayload(payload interface{}, jsonOut bool, header string) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	if jsonOut {
		fmt.Println(string(data))
	} else {
		fmt.Printf("%s:\n%s\n", header, string(data))
	}
	return nil
}
