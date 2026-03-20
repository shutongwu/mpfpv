package web

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	log "github.com/sirupsen/logrus"
)

//go:embed static
var staticFS embed.FS

// --- Data types returned by APIs ---

// ClientInfo is a summary of a connected client (server-side).
type ClientInfo struct {
	ClientID  uint16 `json:"clientID"`
	VirtualIP string `json:"virtualIP"`
	SendMode  string `json:"sendMode"`
	Online    bool   `json:"online"`
	LastSeen  string `json:"lastSeen"`
	AddrCount int    `json:"addrCount"`
}

// ClientDetailInfo includes full address details for a single client.
type ClientDetailInfo struct {
	ClientInfo
	Addrs []AddrDetail `json:"addrs"`
}

// AddrDetail describes a single source address of a client.
type AddrDetail struct {
	Addr     string `json:"addr"`
	LastSeen string `json:"lastSeen"`
}

// StatusInfo is the client's own connection status.
type StatusInfo struct {
	Connected bool   `json:"connected"`
	VirtualIP string `json:"virtualIP"`
	ClientID  uint16 `json:"clientID"`
	SendMode  string `json:"sendMode"`
}

// InterfaceStatus describes a single network interface (client-side).
type InterfaceStatus struct {
	Name     string `json:"name"`
	LocalIP  string `json:"localIP"`
	Status   string `json:"status"`   // "active", "suspect", "down"
	RTT      string `json:"rtt"`      // e.g. "12ms"
	IsActive bool   `json:"isActive"` // failover: marks the current active card
}

// RouteEntry is a virtualIP -> clientID mapping (server-side).
type RouteEntry struct {
	VirtualIP string `json:"virtualIP"`
	ClientID  uint16 `json:"clientID"`
}

// --- Interfaces that Server/Client must implement ---

// ServerAPI exposes server state to the Web UI.
type ServerAPI interface {
	GetClients() []ClientInfo
	GetClient(id uint16) *ClientDetailInfo
	DeleteClient(id uint16) error
	GetRoutes() []RouteEntry
}

// ClientAPI exposes client state to the Web UI.
type ClientAPI interface {
	GetStatus() StatusInfo
	GetInterfaces() []InterfaceStatus
	SetInterfaceEnabled(name string, enabled bool) error
	SetSendMode(mode string) error
}

// --- HTTP handler ---

// NewHandler creates an http.Handler that serves the Web UI and JSON API.
// mode is "server" or "client"; api must implement the corresponding interface.
func NewHandler(mode string, api interface{}) http.Handler {
	mux := http.NewServeMux()

	// Serve static files from the embedded filesystem.
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("web: failed to create sub filesystem: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	if mode == "server" {
		sapi := api.(ServerAPI)

		mux.HandleFunc("/api/clients", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, sapi.GetClients())
		})

		mux.HandleFunc("/api/clients/", func(w http.ResponseWriter, r *http.Request) {
			// Extract client ID from path: /api/clients/{id}
			idStr := strings.TrimPrefix(r.URL.Path, "/api/clients/")
			idStr = strings.TrimSuffix(idStr, "/")
			id, err := strconv.ParseUint(idStr, 10, 16)
			if err != nil {
				http.Error(w, "invalid client ID", http.StatusBadRequest)
				return
			}
			clientID := uint16(id)

			switch r.Method {
			case http.MethodGet:
				info := sapi.GetClient(clientID)
				if info == nil {
					http.Error(w, "client not found", http.StatusNotFound)
					return
				}
				writeJSON(w, info)

			case http.MethodDelete:
				if err := sapi.DeleteClient(clientID); err != nil {
					http.Error(w, err.Error(), http.StatusNotFound)
					return
				}
				writeJSON(w, map[string]string{"status": "deleted"})

			default:
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			}
		})

		mux.HandleFunc("/api/routes", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, sapi.GetRoutes())
		})

	} else {
		capi := api.(ClientAPI)

		mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, capi.GetStatus())
		})

		mux.HandleFunc("/api/interfaces", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			writeJSON(w, capi.GetInterfaces())
		})

		mux.HandleFunc("/api/interfaces/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Path: /api/interfaces/{name}/{action}
			// action: "enable" or "disable"
			rest := strings.TrimPrefix(r.URL.Path, "/api/interfaces/")
			parts := strings.SplitN(rest, "/", 2)
			if len(parts) != 2 {
				http.Error(w, "expected /api/interfaces/{name}/{enable|disable}", http.StatusBadRequest)
				return
			}
			name := parts[0]
			action := parts[1]

			var enabled bool
			switch action {
			case "enable":
				enabled = true
			case "disable":
				enabled = false
			default:
				http.Error(w, "action must be 'enable' or 'disable'", http.StatusBadRequest)
				return
			}

			if err := capi.SetInterfaceEnabled(name, enabled); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]string{"status": "ok"})
		})

		mux.HandleFunc("/api/sendmode", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			var req struct {
				Mode string `json:"mode"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			if err := capi.SetSendMode(req.Mode); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			writeJSON(w, map[string]string{"status": "ok"})
		})
	}

	return mux
}

// StartWebUI starts the Web UI HTTP server on the given address.
func StartWebUI(addr string, handler http.Handler) error {
	log.Infof("web: starting Web UI on %s", addr)
	return http.ListenAndServe(addr, handler)
}

// writeJSON serializes v to JSON and writes it to the ResponseWriter.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Warnf("web: failed to encode JSON response: %v", err)
	}
}

