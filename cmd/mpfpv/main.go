package main

import (
	"context"
	"flag"
	"os/signal"
	"syscall"

	"github.com/cloud/mpfpv/internal/client"
	"github.com/cloud/mpfpv/internal/config"
	log "github.com/sirupsen/logrus"
)

func main() {
	configPath := flag.String("config", "mpfpv.yml", "config file path")
	verbose := flag.Bool("v", false, "enable debug logging")
	flag.Parse()

	if *verbose {
		log.SetLevel(log.DebugLevel)
	}

	cfg, err := config.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	switch cfg.Mode {
	case "server":
		// Phase 1: server not yet implemented
		log.Fatal("server mode not yet implemented")
	case "client":
		c, err := client.New(cfg)
		if err != nil {
			log.Fatalf("failed to create client: %v", err)
		}
		if err := c.Run(ctx); err != nil {
			log.Fatalf("client error: %v", err)
		}
	default:
		log.Fatalf("unknown mode: %q", cfg.Mode)
	}
}
