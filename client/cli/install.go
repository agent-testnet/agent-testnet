package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

const (
	firecrackerVersion = "v1.14.0"
	kernelFilename     = "vmlinux-5.10.bin"
)

func kernelURL(arch string) string {
	return fmt.Sprintf("https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.10/%s/vmlinux-5.10.223", arch)
}

func newInstallCmd() *cobra.Command {
	var rootfsURL string

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Download Firecracker, kernel, and rootfs for agent VMs",
		Long:  "Downloads all VM dependencies to ~/.testnet/bin/. Run once before launching agents.",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, _ := os.UserHomeDir()
			binDir := filepath.Join(home, ".testnet", "bin")
			if err := os.MkdirAll(binDir, 0o755); err != nil {
				return err
			}

			arch := runtime.GOARCH
			if arch == "amd64" {
				arch = "x86_64"
			} else if arch == "arm64" {
				arch = "aarch64"
			}

			fmt.Println("Checking prerequisites...")
			if err := checkPrereqs(); err != nil {
				return err
			}

			fcPath := filepath.Join(binDir, "firecracker")
			if _, err := os.Stat(fcPath); err != nil {
				fmt.Printf("Downloading Firecracker %s (%s)...\n", firecrackerVersion, arch)
				if err := downloadFirecracker(fcPath, arch); err != nil {
					return fmt.Errorf("download firecracker: %w", err)
				}
				fmt.Printf("  Saved to %s\n", fcPath)
			} else {
				fmt.Printf("  Firecracker already installed: %s\n", fcPath)
			}

			kernelPath := filepath.Join(binDir, kernelFilename)
			if _, err := os.Stat(kernelPath); err != nil {
				fmt.Printf("Downloading kernel %s (%s)...\n", kernelFilename, arch)
				if err := downloadFile(kernelPath, kernelURL(arch)); err != nil {
					return fmt.Errorf("download kernel: %w", err)
				}
				fmt.Printf("  Saved to %s\n", kernelPath)
			} else {
				fmt.Printf("  Kernel already installed: %s\n", kernelPath)
			}

			rootfsPath := filepath.Join(binDir, "rootfs.ext4")
			if _, err := os.Stat(rootfsPath); err != nil {
				if rootfsURL == "" {
					fmt.Println("\n  No rootfs found. Provide a URL with --rootfs-url or build one with:")
					fmt.Printf("    sudo bash scripts/gen-rootfs.sh %s\n", rootfsPath)
				} else {
					fmt.Printf("Downloading rootfs from %s...\n", rootfsURL)
					if err := downloadRootFS(rootfsPath, rootfsURL); err != nil {
						return fmt.Errorf("download rootfs: %w", err)
					}
					fmt.Printf("  Saved to %s\n", rootfsPath)
				}
			} else {
				fmt.Printf("  Rootfs already installed: %s\n", rootfsPath)
			}

			fmt.Println("\nInstallation complete!")
			return nil
		},
	}

	cmd.Flags().StringVar(&rootfsURL, "rootfs-url", "", "URL to download pre-built rootfs (gzipped or plain ext4)")
	return cmd
}

func checkPrereqs() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		fmt.Println("  WARNING: /dev/kvm not found — Firecracker requires KVM.")
		fmt.Println("           Ensure you're on a bare-metal or nested-virt-enabled host.")
	} else {
		fmt.Println("  /dev/kvm: OK")
	}

	missing := []string{}
	for _, tool := range []string{"wg", "wg-quick", "iptables"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		fmt.Printf("  WARNING: missing tools: %s\n", strings.Join(missing, ", "))
		fmt.Println("  Install with: sudo apt-get install -y wireguard-tools iptables")
	} else {
		fmt.Println("  wireguard-tools + iptables: OK")
	}
	return nil
}

func downloadFirecracker(destPath, arch string) error {
	url := fmt.Sprintf(
		"https://github.com/firecracker-microvm/firecracker/releases/download/%s/firecracker-%s-%s.tgz",
		firecrackerVersion, firecrackerVersion, arch)

	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	defer gz.Close()

	// The tgz contains release-<ver>-<arch>/firecracker-<ver>-<arch>
	binaryName := fmt.Sprintf("firecracker-%s-%s", firecrackerVersion, arch)
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("firecracker binary not found in archive")
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if filepath.Base(hdr.Name) == binaryName {
			f, err := os.Create(destPath)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				os.Remove(destPath)
				return err
			}
			f.Close()
			return os.Chmod(destPath, 0o755)
		}
	}
}

func downloadFile(destPath, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, destPath)
}

func downloadRootFS(destPath, url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}

	tmp := destPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	var reader io.Reader = resp.Body
	if strings.HasSuffix(url, ".gz") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			f.Close()
			os.Remove(tmp)
			return fmt.Errorf("decompress rootfs: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	if _, err := io.Copy(f, reader); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, destPath)
}
