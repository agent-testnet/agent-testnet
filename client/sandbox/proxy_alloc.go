package sandbox

import (
	"fmt"
	"net"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/config"
)

// ProxyAllocator hands out IP addresses for client-side per-VM passthrough
// proxies, in two flavours:
//
//   - Public: host-wide allocations from config.ClientPassthroughSubnet
//     (default 83.150.255.0/24). These look like normal public IPs to the
//     agent VM, which matters for agents that refuse to fetch URLs whose
//     hostnames resolve to RFC1918 / "special-use" IPs (SSRF guards).
//   - Private: per-VM allocations from 172.16.<vmIndex>.<offset>..254. The
//     agent VM has a connected route for its own /24, so private proxies
//     never leave the TAP, but the source IP is obviously private.
//
// The allocator does NOT persist anything itself — the caller (daemon)
// rehydrates it on startup from the on-disk per-agent proxy state by calling
// Claim() for each previously-assigned IP. This keeps a single source of
// truth: the per-agent proxies.json file.
type ProxyAllocator struct {
	mu          sync.Mutex
	used        map[string]struct{}
	publicCIDR  *net.IPNet
	publicStart byte // first usable host octet (skip .0)
	publicEnd   byte // last usable host octet (skip broadcast .255 of /24)
}

// NewProxyAllocator constructs a new allocator backed by
// config.ClientPassthroughSubnet for public allocations.
func NewProxyAllocator() (*ProxyAllocator, error) {
	_, cidr, err := net.ParseCIDR(config.ClientPassthroughSubnet)
	if err != nil {
		return nil, fmt.Errorf("parse client passthrough subnet %q: %w",
			config.ClientPassthroughSubnet, err)
	}
	ones, bits := cidr.Mask.Size()
	if bits-ones < 8 {
		return nil, fmt.Errorf(
			"client passthrough subnet %q must be at least a /24",
			config.ClientPassthroughSubnet)
	}
	return &ProxyAllocator{
		used:        make(map[string]struct{}),
		publicCIDR:  cidr,
		publicStart: 1,   // skip network address
		publicEnd:   254, // skip broadcast
	}, nil
}

// Claim marks an IP as in-use without doing a fresh allocation. The daemon
// calls this for every IP it loads from per-agent proxies.json so subsequent
// AllocatePublic / AllocatePrivate calls don't collide. Idempotent.
func (a *ProxyAllocator) Claim(ip string) {
	if ip == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.used[ip] = struct{}{}
}

// Release frees a previously-allocated IP so it can be re-handed out.
func (a *ProxyAllocator) Release(ip string) {
	if ip == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.used, ip)
}

// IsClaimed reports whether an IP is currently in use.
func (a *ProxyAllocator) IsClaimed(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.used[ip]
	return ok
}

// AllocatePublic returns the first free IP in config.ClientPassthroughSubnet
// (host-wide) and records it as in-use. Errors if the range is exhausted.
func (a *ProxyAllocator) AllocatePublic() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	base := a.publicCIDR.IP.To4()
	// The third octet is fixed for a /24; for larger subnets we walk only the
	// last /24 (matches the documented reservation semantics).
	for last := a.publicStart; last <= a.publicEnd; last++ {
		ip := net.IPv4(base[0], base[1], base[2], last).String()
		if _, taken := a.used[ip]; taken {
			continue
		}
		a.used[ip] = struct{}{}
		return ip, nil
	}
	return "", fmt.Errorf("public proxy range %s exhausted",
		config.ClientPassthroughSubnet)
}

// AllocatePrivate returns the first free IP from
// 172.16.<vmIndex>.<offset>..254 (where offset = ClientPrivateProxyOffset).
// vmIndex must match the VM's per-VM /24 (see client/sandbox/network.go).
func (a *ProxyAllocator) AllocatePrivate(vmIndex int) (string, error) {
	if vmIndex < 1 || vmIndex > 254 {
		return "", fmt.Errorf("vmIndex %d out of range [1,254]", vmIndex)
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	for last := config.ClientPrivateProxyOffset; last <= 254; last++ {
		ip := fmt.Sprintf("172.16.%d.%d", vmIndex, last)
		if _, taken := a.used[ip]; taken {
			continue
		}
		a.used[ip] = struct{}{}
		return ip, nil
	}
	return "", fmt.Errorf("private proxy range 172.16.%d.%d..254 exhausted",
		vmIndex, config.ClientPrivateProxyOffset)
}

// Allocate dispatches to AllocatePublic or AllocatePrivate based on
// visibility (api.ProxyVisibilityPublic / api.ProxyVisibilityPrivate).
func (a *ProxyAllocator) Allocate(visibility string, vmIndex int) (string, error) {
	switch visibility {
	case "public":
		return a.AllocatePublic()
	case "private":
		return a.AllocatePrivate(vmIndex)
	default:
		return "", fmt.Errorf("unknown proxy visibility %q (use 'public' or 'private')",
			visibility)
	}
}
