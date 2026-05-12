package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/agent-testnet/agent-testnet/client/daemon"
	"github.com/agent-testnet/agent-testnet/pkg/api"
)

func newDaemonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the background daemon",
	}

	cmd.AddCommand(newDaemonStartCmd())
	cmd.AddCommand(newDaemonStopCmd())
	cmd.AddCommand(newDaemonStatusCmd())

	return cmd
}

func newDaemonStartCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in foreground",
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := daemon.New(cfg)
			if err != nil {
				return err
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				fmt.Println("\nShutting down daemon...")
				cancel()
			}()

			return d.Run(ctx)
		},
	}
}

func newDaemonStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{Command: "shutdown"})
			if err != nil {
				return fmt.Errorf("daemon not running or unreachable: %w", err)
			}
			if !resp.OK {
				return fmt.Errorf("daemon error: %s", resp.Error)
			}
			fmt.Println("Daemon stopped.")
			return nil
		},
	}
}

func newDaemonStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			resp, err := daemonRequest(api.DaemonRequest{Command: "status"})
			if err != nil {
				fmt.Println("Daemon: not running")
				return nil
			}
			if !resp.OK {
				return fmt.Errorf("daemon error: %s", resp.Error)
			}

			data, _ := json.MarshalIndent(resp.Payload, "", "  ")
			fmt.Printf("Daemon status:\n%s\n", string(data))
			return nil
		},
	}
}

func daemonRequest(req api.DaemonRequest) (*api.DaemonResponse, error) {
	socketPath := "/var/run/testnet-client.sock"
	if cfg != nil && cfg.Daemon.Socket != "" {
		socketPath = cfg.Daemon.Socket
	}

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}

	var resp api.DaemonResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
