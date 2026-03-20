package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloud/mpfpv/internal/client"
	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/server"
	"github.com/cloud/mpfpv/internal/web"
	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "show version and exit")
	configPath := flag.String("config", "mpfpv.yml", "config file path")
	verbose := flag.Bool("v", false, "enable debug logging")
	flag.Parse()

	if *showVersion {
		fmt.Println("mpfpv version", version)
		os.Exit(0)
	}

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
		s, err := server.New(cfg)
		if err != nil {
			log.Fatalf("failed to create server: %v", err)
		}
		// Start Web UI if configured.
		if cfg.Server.WebUI != "" {
			handler := web.NewHandler("server", s)
			go func() {
				if err := web.StartWebUI(cfg.Server.WebUI, handler); err != nil {
					log.Fatalf("web UI error: %v", err)
				}
			}()
		}
		log.Infof("mpfpv server %s starting", version)
		if err := s.Run(ctx); err != nil {
			log.Fatalf("server error: %v", err)
		}
		log.Info("server stopped gracefully")
	case "client":
		c, err := client.New(cfg)
		if err != nil {
			log.Fatalf("failed to create client: %v", err)
		}
		// Start Web UI if configured.
		if cfg.Client.WebUI != "" {
			handler := web.NewHandler("client", c)
			go func() {
				if err := web.StartWebUI(cfg.Client.WebUI, handler); err != nil {
					log.Fatalf("web UI error: %v", err)
				}
			}()
		}
		log.Infof("mpfpv client %s starting (clientID=%d)", version, cfg.Client.ClientID)
		if err := c.Run(ctx); err != nil {
			log.Fatalf("client error: %v", err)
		}
		log.Info("client stopped gracefully")
	default:
		log.Fatalf("unknown mode: %q", cfg.Mode)
	}
}
