package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/agent-testnet/agent-testnet/client/daemon"
	"github.com/agent-testnet/agent-testnet/pkg/api"
	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/wg"
)

const wgInterfaceName = "wg-testnet"

func newSetupCmd() *cobra.Command {
	var (
		serverURL     string
		joinToken     string
		caFingerprint string
	)

	cmd := &cobra.Command{
		Use:   "setup",
		Short: "Register with the testnet server and bring up the WireGuard tunnel",
		RunE: func(cmd *cobra.Command, args []string) error {
			if serverURL == "" || joinToken == "" {
				return fmt.Errorf("--server-url and --join-token are required")
			}

			fmt.Println("Checking prerequisites...")
			if _, err := os.Stat("/dev/kvm"); err != nil {
				fmt.Println("  WARNING: /dev/kvm not found. Firecracker requires KVM support.")
			} else {
				fmt.Println("  /dev/kvm: OK")
			}

			fmt.Println("Generating WireGuard keypair...")
			privKey, pubKey, err := wg.GenerateClientKeyPair()
			if err != nil {
				return fmt.Errorf("generate WG keypair: %w", err)
			}
			fmt.Printf("  Public key: %s\n", pubKey)

			fmt.Printf("Registering with server at %s...\n", serverURL)
			var client *api.ServerClient
			if caFingerprint != "" {
				client = api.NewServerClientWithFingerprint(serverURL, caFingerprint)
			} else {
				client = api.NewServerClient(serverURL, nil)
			}
			resp, err := client.Register(joinToken, &api.RegisterRequest{
				WGPublicKey: pubKey,
			})
			if err != nil {
				return fmt.Errorf("registration failed: %w", err)
			}

			fmt.Printf("  Client ID: %s\n", resp.ClientID)
			fmt.Printf("  Tunnel CIDR: %s\n", resp.TunnelCIDR)

			home, _ := os.UserHomeDir()
			dataDir := filepath.Join(home, ".testnet", "data")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return err
			}

			state := &daemon.StateFile{
				ServerURL:   serverURL,
				APIToken:    resp.APIToken,
				ClientID:    resp.ClientID,
				TunnelCIDR:  resp.TunnelCIDR,
				DNSIP:       resp.DNSIP,
				WGPrivKey:   privKey,
				WGPubKey:    pubKey,
				ServerWGKey: resp.ServerWGKey,
			}

			clientCfg := &config.ClientConfig{
				Server: config.ClientServerConfig{URL: serverURL},
				Daemon: config.ClientDaemonConfig{
					Socket:   "/var/run/testnet-client.sock",
					DataDir:  "~/.testnet/data",
					WGConfig: "~/.testnet/wg.conf",
				},
				Sandbox: config.ClientSandboxConfig{
					FirecrackerBin: "~/.testnet/bin/firecracker",
					KernelPath:     "~/.testnet/bin/vmlinux-5.10.bin",
					DefaultRootFS:  "~/.testnet/bin/rootfs.ext4",
					DefaultVCPU:    2,
					DefaultMemMB:   4096,
					VMSubnet:       "172.16.0.0/16",
				},
			}

			d, err := daemon.New(clientCfg)
			if err != nil {
				return fmt.Errorf("init daemon: %w", err)
			}

			caCertPEM := []byte(resp.CACert)
			if err := d.SaveRegistration(state, caCertPEM); err != nil {
				return fmt.Errorf("save registration: %w", err)
			}

			configPath := filepath.Join(home, ".testnet", "config.yaml")
			configContent := fmt.Sprintf(`server:
  url: %q

daemon:
  socket: "/var/run/testnet-client.sock"
  data_dir: "~/.testnet/data"
  wg_config: "~/.testnet/wg.conf"

sandbox:
  firecracker_bin: "~/.testnet/bin/firecracker"
  kernel_path: "~/.testnet/bin/vmlinux-5.10.bin"
  default_rootfs: "~/.testnet/bin/rootfs.ext4"
  default_vcpu: 2
  default_mem_mb: 4096
  vm_subnet: "172.16.0.0/16"
`, serverURL)
			if err := os.WriteFile(configPath, []byte(configContent), 0o600); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			// Write WireGuard config and bring up the tunnel
			fmt.Println("Configuring WireGuard tunnel...")
			clientIP, err := writeWGConfig(state, serverURL)
			if err != nil {
				return fmt.Errorf("write WG config: %w", err)
			}

			fmt.Printf("  Bringing up %s...\n", wgInterfaceName)
			if err := bringUpWG(); err != nil {
				return fmt.Errorf("bring up tunnel: %w", err)
			}

			fmt.Printf("  Tunnel IP: %s\n", clientIP)

			// Verify connectivity
			fmt.Println("Verifying tunnel connectivity...")
			serverTunnelIP := "10.99.0.1"
			if err := verifyTunnel(serverTunnelIP); err != nil {
				fmt.Printf("  WARNING: tunnel to %s is not responding\n", serverTunnelIP)
				fmt.Println("")
				fmt.Println("  Possible causes:")
				fmt.Println("    1. UDP port 51820 is blocked by the server's hosting provider firewall")
				fmt.Println("       -> Check your provider's dashboard for a platform-level firewall")
				fmt.Println("       -> Open UDP 51820 (this is separate from iptables on the server)")
				fmt.Println("    2. The tunnel may still be establishing (wait 10s, then: ping 10.99.0.1)")
				fmt.Println("    3. Check server WireGuard: ssh server 'wg show wg0'")
			} else {
				fmt.Printf("  Ping to %s: OK\n", serverTunnelIP)
			}

			fmt.Println("\nSetup complete!")
			fmt.Printf("  Config:  %s\n", configPath)
			fmt.Printf("  Data:    %s\n", dataDir)
			fmt.Printf("  Tunnel:  %s (interface %s)\n", clientIP, wgInterfaceName)
			fmt.Println("\nNext steps:")
			fmt.Println("  testnet-client install       # download Firecracker + kernel + rootfs")
			fmt.Println("  testnet-client agent launch   # launch an agent VM")
			return nil
		},
	}

	cmd.Flags().StringVar(&serverURL, "server-url", "", "testnet server URL (https://...)")
	cmd.Flags().StringVar(&joinToken, "join-token", "", "join token from server")
	cmd.Flags().StringVar(&caFingerprint, "ca-fingerprint", "", "SHA-256 fingerprint of server TLS cert (hex) for bootstrap verification")
	return cmd
}

func writeWGConfig(state *daemon.StateFile, serverURL string) (string, error) {
	_, ipNet, err := net.ParseCIDR(state.TunnelCIDR)
	if err != nil {
		return "", fmt.Errorf("parse tunnel CIDR: %w", err)
	}
	clientIP := make(net.IP, len(ipNet.IP))
	copy(clientIP, ipNet.IP)
	clientIP[3] = 1

	serverHost := serverURL
	serverHost = strings.TrimPrefix(serverHost, "https://")
	serverHost = strings.TrimPrefix(serverHost, "http://")
	host := strings.Split(serverHost, ":")[0]
	endpoint := host + ":51820"

	wgConfDir := "/etc/wireguard"
	if err := os.MkdirAll(wgConfDir, 0o700); err != nil {
		return "", err
	}

	wgConf := fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/24

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 10.99.0.0/16, 83.150.0.0/16
PersistentKeepalive = 25
`, state.WGPrivKey, clientIP.String(), state.ServerWGKey, endpoint)

	confPath := filepath.Join(wgConfDir, wgInterfaceName+".conf")
	if err := os.WriteFile(confPath, []byte(wgConf), 0o600); err != nil {
		return "", err
	}

	return clientIP.String(), nil
}

func bringUpWG() error {
	// Tear down first in case it's already up (idempotent)
	exec.Command("wg-quick", "down", wgInterfaceName).Run()

	cmd := exec.Command("wg-quick", "up", wgInterfaceName)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func verifyTunnel(serverIP string) error {
	cmd := exec.Command("ping", "-c", "3", "-W", "2", serverIP)
	return cmd.Run()
}
