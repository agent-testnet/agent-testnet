package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/agent-testnet/agent-testnet/pkg/config"
	"github.com/agent-testnet/agent-testnet/server/controlplane"
	"github.com/agent-testnet/agent-testnet/server/dns"
	"github.com/agent-testnet/agent-testnet/server/router"
	"github.com/agent-testnet/agent-testnet/server/wg"
)

func main() {
	configPath := flag.String("config", "./configs/server.yaml", "path to server config")
	flag.Parse()

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cp, err := controlplane.New(cfg)
	if err != nil {
		log.Fatalf("failed to init control plane: %v", err)
	}

	wgEndpoint, err := wg.NewEndpoint(cfg, cp)
	if err != nil {
		log.Fatalf("failed to init wireguard endpoint: %v", err)
	}
	cp.SetPeerManager(wgEndpoint)

	dnsServer, err := dns.NewServer(cfg, cp)
	if err != nil {
		log.Fatalf("failed to init dns server: %v", err)
	}

	rt, err := router.New(cfg, cp)
	if err != nil {
		log.Fatalf("failed to init router: %v", err)
	}

	go func() {
		if err := cp.ListenAndServe(); err != nil {
			log.Fatalf("control plane error: %v", err)
		}
	}()

	go func() {
		if err := wgEndpoint.Start(ctx); err != nil {
			log.Fatalf("wireguard error: %v", err)
		}
	}()

	go func() {
		if err := dnsServer.Start(ctx); err != nil {
			log.Fatalf("dns error: %v", err)
		}
	}()

	go func() {
		if err := rt.Start(ctx); err != nil {
			log.Fatalf("router error: %v", err)
		}
	}()

	log.Println("testnet-server started")

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGHUP:
			log.Println("SIGHUP received, reloading nodes.yaml...")
			if err := cp.ReloadNodes(); err != nil {
				log.Printf("failed to reload nodes: %v", err)
			}
		case syscall.SIGINT, syscall.SIGTERM:
			log.Println("shutting down...")
			cancel()
			rt.Cleanup()
			return
		}
	}
}
