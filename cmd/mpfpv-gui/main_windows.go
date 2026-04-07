//go:build windows

package main

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/cloud/mpfpv/internal/config"
	"github.com/cloud/mpfpv/internal/web"
	"github.com/jchv/go-webview2"
	log "github.com/sirupsen/logrus"
)

var version = "dev"

func main() {
	log.SetLevel(log.InfoLevel)

	// --- 1. Locate config file (same directory as the exe). ---
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("gui: failed to get executable path: %v", err)
	}
	cfgPath := filepath.Join(filepath.Dir(exePath), "mpfpv.yml")

	var cfg *config.Config
	cfg, err = config.LoadConfig(cfgPath)
	if err != nil {
		// No valid config yet. Create a minimal default for the GUI to edit.
		log.Warnf("gui: failed to load config from %s: %v (will show config editor)", cfgPath, err)
		cfg = &config.Config{
			Mode:    "client",
			TeamKey: "",
			Client: &config.ClientConfig{
				ClientID: 1,
				SendMode: "redundant",
				MTU:      1400,
			},
		}
	}

	// Force client mode for the GUI.
	cfg.Mode = "client"

	// --- 2. Create the GUI controller. ---
	ctrl := NewController(cfgPath, cfg)

	// --- 3. Start local Web UI HTTP server on a random port. ---
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("gui: failed to listen on localhost: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	log.Infof("gui: Web UI listening on 127.0.0.1:%d", port)

	// Create web handler in "client" mode with the GUI controller.
	// We pass a DynamicClientAPI that delegates to the controller's current client.
	dynAPI := &DynamicClientAPI{ctrl: ctrl}
	handler := web.NewHandler("client", dynAPI, ctrl)

	go func() {
		srv := &http.Server{Handler: handler}
		if err := srv.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Fatalf("gui: web server error: %v", err)
		}
	}()

	// --- 4. Open WebView2 window. ---
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title:  "mpfpv Client",
			Width:  900,
			Height: 650,
		},
	})
	if w == nil {
		log.Fatal("gui: WebView2 runtime is not available. Please install Microsoft Edge WebView2 Runtime.")
	}
	defer w.Destroy()

	w.Navigate(fmt.Sprintf("http://127.0.0.1:%d", port))
	w.Run() // Blocks until window is closed.

	// --- 5. Cleanup: stop client if running. ---
	log.Info("gui: window closed, shutting down")
	if err := ctrl.Disconnect(); err != nil {
		log.WithError(err).Warn("gui: error during disconnect")
	}
}

// DynamicClientAPI delegates ClientAPI calls to the controller's current client.
// When no client is running, it returns sensible defaults.
type DynamicClientAPI struct {
	ctrl *Controller
	mu   sync.Mutex
}

func (d *DynamicClientAPI) GetStatus() web.StatusInfo {
	cli := d.ctrl.GetClient()
	if cli == nil {
		d.ctrl.mu.Lock()
		cfg := d.ctrl.cfg
		d.ctrl.mu.Unlock()

		sm := "redundant"
		var cid uint16
		if cfg != nil && cfg.Client != nil {
			sm = cfg.Client.SendMode
			cid = cfg.Client.ClientID
		}
		return web.StatusInfo{
			Connected: false,
			VirtualIP: "",
			ClientID:  cid,
			SendMode:  sm,
		}
	}
	return cli.GetStatus()
}

func (d *DynamicClientAPI) GetInterfaces() []web.InterfaceStatus {
	cli := d.ctrl.GetClient()
	if cli == nil {
		return []web.InterfaceStatus{}
	}
	return cli.GetInterfaces()
}

func (d *DynamicClientAPI) SetInterfaceEnabled(name string, enabled bool) error {
	cli := d.ctrl.GetClient()
	if cli == nil {
		return fmt.Errorf("client not running")
	}
	return cli.SetInterfaceEnabled(name, enabled)
}

func (d *DynamicClientAPI) SetSendMode(mode string) error {
	cli := d.ctrl.GetClient()
	if cli == nil {
		return fmt.Errorf("client not running")
	}
	return cli.SetSendMode(mode)
}
