//go:build windows

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/jchv/go-webview2"
	log "github.com/sirupsen/logrus"
)

var version = "dev"

var defaultConfig = config.Config{
	Mode:    "client",
	TeamKey: "",
	Client: &config.ClientConfig{
		SendMode: "redundant",
		MTU:      1400,
	},
}

func main() {
	log.SetLevel(log.InfoLevel)

	// Check admin privileges; wintun requires it.
	if !isAdmin() {
		relaunchAsAdmin()
		return
	}

	// Locate config in exe directory.
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("gui: failed to get executable path: %v", err)
	}
	cfgPath := filepath.Join(filepath.Dir(exePath), "mpfpv.yml")

	var cfg *config.Config
	cfg, err = config.LoadConfig(cfgPath)
	if err != nil {
		log.Warnf("gui: no config at %s, using defaults", cfgPath)
		c := defaultConfig
		cfg = &c
	}
	cfg.Mode = "client"

	ctrl := NewController(cfgPath, cfg)

	// Start HTTP server.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("gui: failed to listen: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	log.Infof("gui: listening on 127.0.0.1:%d", port)

	handler := NewAPIMux(ctrl)
	go func() {
		srv := &http.Server{Handler: handler}
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gui: server error: %v", err)
		}
	}()

	// Open WebView2 window.
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  fmt.Sprintf("mpfpv %s", version),
			Width:  480,
			Height: 520,
		},
	})
	if w == nil {
		log.Fatal("gui: WebView2 runtime not available. Please install Microsoft Edge WebView2 Runtime.")
	}
	defer w.Destroy()

	w.Navigate(fmt.Sprintf("http://127.0.0.1:%d", port))
	w.Run()

	// Cleanup.
	log.Info("gui: window closed, shutting down")
	ctrl.Disconnect()
}
