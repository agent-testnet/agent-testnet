package controlplane

import (
	"fmt"
	"net"
	"sync"
)

// VIPAllocator manages virtual IP allocation from the VIP subnet (default 83.150.0.0/16).
type VIPAllocator struct {
	mu        sync.Mutex
	subnet    *net.IPNet
	dnsVIP    net.IP
	allocated map[string]net.IP // key (node name or domain) -> VIP
	nextOctet [2]byte           // tracks next available in {subnet}.X.Y
}

func NewVIPAllocator(subnet, dnsVIP string) (*VIPAllocator, error) {
	_, ipNet, err := net.ParseCIDR(subnet)
	if err != nil {
		return nil, fmt.Errorf("parse VIP subnet %s: %w", subnet, err)
	}

	dns := net.ParseIP(dnsVIP)
	if dns == nil {
		return nil, fmt.Errorf("invalid DNS VIP: %s", dnsVIP)
	}

	return &VIPAllocator{
		subnet:    ipNet,
		dnsVIP:    dns.To4(),
		allocated: make(map[string]net.IP),
		nextOctet: [2]byte{0, 2}, // start at {subnet}.0.2 (0.1 is DNS)
	}, nil
}

// DNSVIP returns the reserved DNS virtual IP.
func (v *VIPAllocator) DNSVIP() net.IP {
	return v.dnsVIP
}

// AllocateVIP assigns a VIP to the given key. If already allocated, returns existing.
func (v *VIPAllocator) AllocateVIP(key string) (net.IP, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	if existing, ok := v.allocated[key]; ok {
		return existing, nil
	}

	ip := v.nextIP()
	if ip == nil {
		return nil, fmt.Errorf("VIP subnet exhausted")
	}
	v.allocated[key] = ip
	return ip, nil
}

// GetVIP returns the VIP for a key, or nil if not allocated.
func (v *VIPAllocator) GetVIP(key string) net.IP {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.allocated[key]
}

// ReleaseVIP frees a VIP allocation. Note: the IP is not reused in this
// simple implementation (only new IPs are allocated sequentially).
func (v *VIPAllocator) ReleaseVIP(key string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.allocated, key)
}

// AllAllocations returns a copy of all current VIP allocations.
func (v *VIPAllocator) AllAllocations() map[string]net.IP {
	v.mu.Lock()
	defer v.mu.Unlock()
	result := make(map[string]net.IP, len(v.allocated))
	for k, ip := range v.allocated {
		result[k] = ip
	}
	return result
}

func (v *VIPAllocator) nextIP() net.IP {
	base := v.subnet.IP.To4()
	for {
		ip := net.IPv4(base[0], base[1], v.nextOctet[0], v.nextOctet[1])
		v.nextOctet[1]++
		if v.nextOctet[1] == 0 {
			v.nextOctet[0]++
			if v.nextOctet[0] == 0 {
				return nil // exhausted
			}
		}

		if ip.Equal(v.dnsVIP) {
			continue
		}

		if !v.subnet.Contains(ip) {
			return nil
		}
		return ip
	}
}
