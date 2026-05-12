package sandbox

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// VMConfig specifies how to create a Firecracker VM.
type VMConfig struct {
	ID             string
	VCPU           int
	MemMB          int
	RootFS         string
	KernelPath     string
	FirecrackerBin string
	TAPDevice      string
	GuestIP        string
	GatewayIP      string
	DNSIP          string
	CACertPEM      []byte
	SSHPubKey      string
}

// VM manages a single Firecracker MicroVM.
type VM struct {
	cfg     VMConfig
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	sockDir string
}

// NewVM creates a new VM instance (does not start it).
func NewVM(cfg VMConfig) (*VM, error) {
	if cfg.VCPU == 0 {
		cfg.VCPU = 2
	}
	if cfg.MemMB == 0 {
		cfg.MemMB = 4096
	}

	if _, err := os.Stat(cfg.FirecrackerBin); err != nil {
		return nil, fmt.Errorf("firecracker binary not found: %s", cfg.FirecrackerBin)
	}
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("kernel not found: %s", cfg.KernelPath)
	}
	if _, err := os.Stat(cfg.RootFS); err != nil {
		return nil, fmt.Errorf("rootfs not found: %s", cfg.RootFS)
	}

	sockDir, err := os.MkdirTemp("", "testnet-vm-"+cfg.ID+"-")
	if err != nil {
		return nil, err
	}

	return &VM{
		cfg:     cfg,
		sockDir: sockDir,
	}, nil
}

// VCPU returns the VM's vCPU count.
func (v *VM) VCPU() int { return v.cfg.VCPU }

// MemMB returns the VM's memory in MB.
func (v *VM) MemMB() int { return v.cfg.MemMB }

// Start launches the Firecracker VM.
// It copies the base rootfs to a per-VM file, injects the SSH public key and
// CA certificate, then starts Firecracker against the copy.
func (v *VM) Start() error {
	sockPath := filepath.Join(v.sockDir, "firecracker.sock")

	vmRootFS, err := v.prepareRootFS()
	if err != nil {
		return fmt.Errorf("prepare rootfs: %w", err)
	}

	bootArgs := fmt.Sprintf(
		"console=ttyS0 reboot=k panic=1 random.trust_cpu=on "+
			"root=/dev/vda rw "+
			"ip=%s::%s:255.255.255.0::eth0:off "+
			"nameserver=%s",
		v.cfg.GuestIP, v.cfg.GatewayIP, v.cfg.DNSIP,
	)

	configPath := filepath.Join(v.sockDir, "config.json")
	mac, err := generateMAC()
	if err != nil {
		return fmt.Errorf("generate guest MAC: %w", err)
	}

	configJSON := fmt.Sprintf(`{
  "boot-source": {
    "kernel_image_path": %q,
    "boot_args": %q
  },
  "drives": [
    {
      "drive_id": "rootfs",
      "path_on_host": %q,
      "is_root_device": true,
      "is_read_only": false
    }
  ],
  "machine-config": {
    "vcpu_count": %d,
    "mem_size_mib": %d,
    "smt": false
  },
  "network-interfaces": [
    {
      "iface_id": "eth0",
      "guest_mac": %q,
      "host_dev_name": %q
    }
  ]
}`, v.cfg.KernelPath, bootArgs, vmRootFS, v.cfg.VCPU, v.cfg.MemMB, mac, v.cfg.TAPDevice)

	if err := os.WriteFile(configPath, []byte(configJSON), 0o644); err != nil {
		return fmt.Errorf("write VM config: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel

	v.cmd = exec.CommandContext(ctx, v.cfg.FirecrackerBin,
		"--api-sock", sockPath,
		"--config-file", configPath)

	logFile, err := os.Create(filepath.Join(v.sockDir, "vm.log"))
	if err != nil {
		cancel()
		return err
	}
	v.cmd.Stdout = logFile
	v.cmd.Stderr = logFile

	if err := v.cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start firecracker: %w", err)
	}

	log.Printf("[vm %s] started (PID %d, %d vCPU, %dMB, TAP %s, IP %s)",
		v.cfg.ID, v.cmd.Process.Pid, v.cfg.VCPU, v.cfg.MemMB, v.cfg.TAPDevice, v.cfg.GuestIP)
	return nil
}

// prepareRootFS copies the base rootfs image to a per-VM file and injects
// the SSH public key and CA certificate into the copy.
func (v *VM) prepareRootFS() (string, error) {
	vmRootFS := filepath.Join(v.sockDir, "rootfs.ext4")

	log.Printf("[vm %s] copying rootfs to %s", v.cfg.ID, vmRootFS)
	if err := copyFile(v.cfg.RootFS, vmRootFS); err != nil {
		return "", fmt.Errorf("copy rootfs: %w", err)
	}

	needsMount := v.cfg.SSHPubKey != "" || len(v.cfg.CACertPEM) > 0
	if !needsMount {
		return vmRootFS, nil
	}

	mountDir := filepath.Join(v.sockDir, "mnt")
	if err := os.MkdirAll(mountDir, 0o755); err != nil {
		return "", err
	}

	if err := exec.Command("mount", "-o", "loop", vmRootFS, mountDir).Run(); err != nil {
		return "", fmt.Errorf("mount rootfs: %w", err)
	}
	defer func() {
		if err := exec.Command("umount", mountDir).Run(); err != nil {
			log.Printf("[vm %s] warning: umount failed: %v", v.cfg.ID, err)
		}
	}()

	if v.cfg.SSHPubKey != "" {
		sshDir := filepath.Join(mountDir, "root", ".ssh")
		if err := os.MkdirAll(sshDir, 0o700); err != nil {
			return "", fmt.Errorf("create .ssh dir: %w", err)
		}
		akPath := filepath.Join(sshDir, "authorized_keys")
		if err := os.WriteFile(akPath, []byte(v.cfg.SSHPubKey+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("write authorized_keys: %w", err)
		}
		log.Printf("[vm %s] injected SSH public key", v.cfg.ID)
	}

	if len(v.cfg.CACertPEM) > 0 {
		caDir := filepath.Join(mountDir, "usr", "local", "share", "ca-certificates", "testnet")
		if err := os.MkdirAll(caDir, 0o755); err != nil {
			return "", fmt.Errorf("create CA dir: %w", err)
		}
		caPath := filepath.Join(caDir, "testnet-ca.crt")
		if err := os.WriteFile(caPath, v.cfg.CACertPEM, 0o644); err != nil {
			return "", fmt.Errorf("write CA cert: %w", err)
		}
		if out, err := exec.Command("chroot", mountDir, "update-ca-certificates").CombinedOutput(); err != nil {
			log.Printf("[vm %s] warning: update-ca-certificates: %s: %v", v.cfg.ID, string(out), err)
		}
		log.Printf("[vm %s] injected CA certificate", v.cfg.ID)
	}

	return vmRootFS, nil
}

// generateMAC returns a random locally-administered unicast MAC address.
func generateMAC() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate MAC address: %w", err)
	}
	b[0] = (b[0] | 0x02) & 0xFE // locally administered, unicast
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5]), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// Stop gracefully shuts down the VM.
func (v *VM) Stop() error {
	if v.cancel != nil {
		v.cancel()
	}
	if v.cmd != nil && v.cmd.Process != nil {
		if err := v.cmd.Wait(); err != nil {
			if !strings.Contains(err.Error(), "signal: killed") {
				return err
			}
		}
	}
	os.RemoveAll(v.sockDir)
	log.Printf("[vm %s] stopped", v.cfg.ID)
	return nil
}

// LogPath returns the path to the VM's console log.
func (v *VM) LogPath() string {
	return filepath.Join(v.sockDir, "vm.log")
}
