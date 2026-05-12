package dns

import (
	"context"
	"log"
	"net"
	"strings"
	"time"

	mdns "github.com/miekg/dns"
	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
)

// DomainResolver is the interface for looking up domain -> VIP mappings.
type DomainResolver interface {
	ResolveDomain(domain string) net.IP
}

// Server is the testnet DNS server. It resolves declared domains to VIPs
// and returns NXDOMAIN for everything else. Never forwards to real DNS.
type Server struct {
	cfg      *config.ServerConfig
	resolver DomainResolver

	udpTunnel   *mdns.Server
	tcpTunnel   *mdns.Server
	udpPublic   *mdns.Server
	tcpPublic   *mdns.Server
}

// NewServer creates a testnet DNS server.
func NewServer(cfg *config.ServerConfig, cp *controlplane.ControlPlane) (*Server, error) {
	return &Server{
		cfg:      cfg,
		resolver: cp.Nodes(),
	}, nil
}

// Start launches the DNS server on configured addresses.
func (s *Server) Start(ctx context.Context) error {
	handler := mdns.HandlerFunc(s.handleDNS)

	tunnelAddr := s.cfg.DNS.ListenTunnel
	if tunnelAddr != "" {
		// The tunnel address (e.g. 83.150.0.1:53) may not be bindable yet if
		// the WireGuard interface hasn't finished setup. Retry with backoff.
		go s.listenWithRetry(ctx, tunnelAddr, "udp", "tunnel", handler, &s.udpTunnel)
		go s.listenWithRetry(ctx, tunnelAddr, "tcp", "tunnel", handler, &s.tcpTunnel)
	}

	publicAddr := s.cfg.DNS.ListenPublic
	if publicAddr != "" {
		s.udpPublic = &mdns.Server{Addr: publicAddr, Net: "udp", Handler: handler}
		s.tcpPublic = &mdns.Server{Addr: publicAddr, Net: "tcp", Handler: handler}

		go func() {
			log.Printf("[dns] listening on %s (UDP, public)", publicAddr)
			if err := s.udpPublic.ListenAndServe(); err != nil {
				log.Printf("[dns] UDP public error: %v", err)
			}
		}()
		go func() {
			log.Printf("[dns] listening on %s (TCP, public)", publicAddr)
			if err := s.tcpPublic.ListenAndServe(); err != nil {
				log.Printf("[dns] TCP public error: %v", err)
			}
		}()
	}

	<-ctx.Done()
	s.shutdown()
	return nil
}

func (s *Server) listenWithRetry(ctx context.Context, addr, network, label string, handler mdns.Handler, target **mdns.Server) {
	for attempt := 1; ; attempt++ {
		srv := &mdns.Server{Addr: addr, Net: network, Handler: handler}
		*target = srv

		log.Printf("[dns] listening on %s (%s, %s)", addr, strings.ToUpper(network), label)
		errCh := make(chan error, 1)
		go func() { errCh <- srv.ListenAndServe() }()

		select {
		case err := <-errCh:
			if err == nil {
				return
			}
			if attempt >= 10 {
				log.Printf("[dns] giving up on %s %s after %d attempts: %v", network, addr, attempt, err)
				return
			}
			log.Printf("[dns] %s %s bind failed (attempt %d/10), retrying: %v", network, addr, attempt, err)
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		case <-ctx.Done():
			srv.Shutdown()
			return
		}
	}
}

func (s *Server) shutdown() {
	for _, srv := range []*mdns.Server{s.udpTunnel, s.tcpTunnel, s.udpPublic, s.tcpPublic} {
		if srv != nil {
			srv.Shutdown()
		}
	}
}

func (s *Server) handleDNS(w mdns.ResponseWriter, r *mdns.Msg) {
	msg := new(mdns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	msg.RecursionAvailable = false

	if len(r.Question) == 0 {
		msg.Rcode = mdns.RcodeFormatError
		w.WriteMsg(msg)
		return
	}

	q := r.Question[0]
	name := strings.TrimSuffix(q.Name, ".")

	switch q.Qtype {
	case mdns.TypeA:
		ip := s.resolver.ResolveDomain(name)
		if ip != nil {
			msg.Answer = append(msg.Answer, &mdns.A{
				Hdr: mdns.RR_Header{
					Name:   q.Name,
					Rrtype: mdns.TypeA,
					Class:  mdns.ClassINET,
					Ttl:    60,
				},
				A: ip,
			})
		} else {
			msg.Rcode = mdns.RcodeNameError // NXDOMAIN
		}

	case mdns.TypeAAAA:
		// No IPv6 support in testnet — return empty answer (no error, just no records)
		// This prevents agents from getting confused by NXDOMAIN on AAAA lookups

	default:
		msg.Rcode = mdns.RcodeNameError
	}

	w.WriteMsg(msg)
}
