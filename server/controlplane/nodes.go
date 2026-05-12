package controlplane

import (
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/agent-testnet/agent-testnet/pkg/api"
	"gopkg.in/yaml.v3"
)

var validNodeName = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]{0,61}[a-zA-Z0-9])?$`)

// NodeManager loads and manages the testnet topology from nodes.yaml.
type NodeManager struct {
	mu       sync.RWMutex
	filePath string
	nodes    map[string]*api.Node   // name -> Node
	domains  map[string]*api.Node   // domain -> owning Node
	vipAlloc *VIPAllocator
	onChange []func()               // callbacks for DNS/router reload
}

func NewNodeManager(filePath string, vipAlloc *VIPAllocator) *NodeManager {
	return &NodeManager{
		filePath: filePath,
		nodes:    make(map[string]*api.Node),
		domains:  make(map[string]*api.Node),
		vipAlloc: vipAlloc,
	}
}

// OnChange registers a callback invoked after nodes are loaded/reloaded.
func (nm *NodeManager) OnChange(fn func()) {
	nm.mu.Lock()
	defer nm.mu.Unlock()
	nm.onChange = append(nm.onChange, fn)
}

// Load reads nodes.yaml and sets up VIP allocations.
func (nm *NodeManager) Load() error {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	data, err := os.ReadFile(nm.filePath)
	if err != nil {
		return fmt.Errorf("read nodes file %s: %w", nm.filePath, err)
	}

	var cfg api.NodesConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("parse nodes file %s: %w", nm.filePath, err)
	}

	if err := nm.validateNodes(cfg.Nodes); err != nil {
		return err
	}

	// Sort nodes by name for deterministic VIP allocation
	sort.Slice(cfg.Nodes, func(i, j int) bool {
		return cfg.Nodes[i].Name < cfg.Nodes[j].Name
	})

	newNodes := make(map[string]*api.Node, len(cfg.Nodes))
	newDomains := make(map[string]*api.Node)

	for i := range cfg.Nodes {
		node := &cfg.Nodes[i]

		vip, err := nm.vipAlloc.AllocateVIP(node.Name)
		if err != nil {
			return fmt.Errorf("allocate VIP for node %s: %w", node.Name, err)
		}
		node.VIP = vip
		newNodes[node.Name] = node

		// Auto-name: {name}.testnet
		autoName := node.Name + ".testnet"
		newDomains[autoName] = node

		for _, domain := range node.Domains {
			newDomains[domain] = node
		}
	}

	nm.nodes = newNodes
	nm.domains = newDomains

	log.Printf("[nodes] loaded %d nodes, %d domain mappings", len(newNodes), len(newDomains))
	for _, node := range cfg.Nodes {
		log.Printf("[nodes]   %s -> VIP %s (domains: %v)", node.Name, node.VIP, node.Domains)
	}

	for _, fn := range nm.onChange {
		go fn()
	}

	return nil
}

func (nm *NodeManager) validateNodes(nodes []api.Node) error {
	names := make(map[string]bool)
	domains := make(map[string]string)

	for _, n := range nodes {
		if n.Name == "" {
			return fmt.Errorf("node missing name")
		}
		if !validNodeName.MatchString(n.Name) {
			return fmt.Errorf("node %q: name must be alphanumeric (plus .-_), 1-63 chars, no path separators", n.Name)
		}
		if strings.Contains(n.Name, "..") {
			return fmt.Errorf("node %q: name must not contain '..'", n.Name)
		}
		if n.Address == "" {
			return fmt.Errorf("node %s: missing address", n.Name)
		}
		host, port, err := net.SplitHostPort(n.Address)
		if err != nil {
			return fmt.Errorf("node %s: address must be host:port, got %q", n.Name, n.Address)
		}
		if host == "" || port == "" {
			return fmt.Errorf("node %s: address must have non-empty host and port", n.Name)
		}
		if names[n.Name] {
			return fmt.Errorf("duplicate node name: %s", n.Name)
		}
		names[n.Name] = true

		for _, d := range n.Domains {
			if owner, exists := domains[d]; exists {
				return fmt.Errorf("duplicate domain %s: claimed by both %s and %s", d, owner, n.Name)
			}
			domains[d] = n.Name
		}
	}
	return nil
}

// GetNode returns a node by name, or nil.
func (nm *NodeManager) GetNode(name string) *api.Node {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	return nm.nodes[name]
}

// ListNodes returns all declared nodes.
func (nm *NodeManager) ListNodes() []*api.Node {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	result := make([]*api.Node, 0, len(nm.nodes))
	for _, n := range nm.nodes {
		result = append(result, n)
	}
	return result
}

// ResolveDomain looks up a domain (or auto-name) and returns the owning node's VIP.
func (nm *NodeManager) ResolveDomain(domain string) net.IP {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	if node, ok := nm.domains[domain]; ok {
		return node.VIP
	}
	return nil
}

// AllDomainMappings returns every domain/auto-name with its VIP and owning node name.
func (nm *NodeManager) AllDomainMappings() []api.DomainMapping {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	var result []api.DomainMapping
	for domain, node := range nm.domains {
		result = append(result, api.DomainMapping{
			Domain: domain,
			VIP:    node.VIP.String(),
			Node:   node.Name,
		})
	}
	return result
}

// AllNodes returns node info suitable for the API.
func (nm *NodeManager) AllNodes() []api.NodeInfo {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	var result []api.NodeInfo
	for _, node := range nm.nodes {
		result = append(result, api.NodeInfo{
			Name:    node.Name,
			VIP:     node.VIP.String(),
			Domains: node.Domains,
		})
	}
	return result
}

// VIPToAddress returns the real address for a given VIP by scanning all nodes.
func (nm *NodeManager) VIPToAddress(vip net.IP) (string, bool) {
	nm.mu.RLock()
	defer nm.mu.RUnlock()
	for _, node := range nm.nodes {
		if node.VIP.Equal(vip) {
			return node.Address, true
		}
	}
	return "", false
}
