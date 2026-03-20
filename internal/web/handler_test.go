package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Mock implementations ---

type mockServerAPI struct {
	clients    []ClientInfo
	detail     *ClientDetailInfo
	routes     []RouteEntry
	deletedIDs []uint16
}

func (m *mockServerAPI) GetClients() []ClientInfo {
	return m.clients
}

func (m *mockServerAPI) GetClient(id uint16) *ClientDetailInfo {
	if m.detail != nil && m.detail.ClientID == id {
		return m.detail
	}
	return nil
}

func (m *mockServerAPI) DeleteClient(id uint16) error {
	for _, c := range m.clients {
		if c.ClientID == id {
			m.deletedIDs = append(m.deletedIDs, id)
			return nil
		}
	}
	return fmt.Errorf("client %d not found", id)
}

func (m *mockServerAPI) GetRoutes() []RouteEntry {
	return m.routes
}

type mockClientAPI struct {
	status     StatusInfo
	interfaces []InterfaceStatus
	sendMode   string
	lastIface  string
	lastEnable bool
}

func (m *mockClientAPI) GetStatus() StatusInfo {
	return m.status
}

func (m *mockClientAPI) GetInterfaces() []InterfaceStatus {
	return m.interfaces
}

func (m *mockClientAPI) SetInterfaceEnabled(name string, enabled bool) error {
	m.lastIface = name
	m.lastEnable = enabled
	return nil
}

func (m *mockClientAPI) SetSendMode(mode string) error {
	if mode != "redundant" && mode != "failover" {
		return fmt.Errorf("invalid mode %q", mode)
	}
	m.sendMode = mode
	return nil
}

// --- Server mode tests ---

func TestServerGetClients(t *testing.T) {
	api := &mockServerAPI{
		clients: []ClientInfo{
			{ClientID: 1, VirtualIP: "10.99.0.1", SendMode: "redundant", Online: true, LastSeen: "2026-01-01T00:00:00Z", AddrCount: 2},
			{ClientID: 2, VirtualIP: "10.99.0.2", SendMode: "failover", Online: false, LastSeen: "2026-01-01T00:00:00Z", AddrCount: 1},
		},
	}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/clients")
	if err != nil {
		t.Fatalf("GET /api/clients: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("expected application/json, got %q", ct)
	}

	var clients []ClientInfo
	if err := json.NewDecoder(resp.Body).Decode(&clients); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("expected 2 clients, got %d", len(clients))
	}
	if clients[0].ClientID != 1 && clients[1].ClientID != 1 {
		t.Errorf("expected clientID 1 in results")
	}
}

func TestServerGetClientDetail(t *testing.T) {
	api := &mockServerAPI{
		detail: &ClientDetailInfo{
			ClientInfo: ClientInfo{ClientID: 1, VirtualIP: "10.99.0.1", SendMode: "redundant", Online: true, AddrCount: 1},
			Addrs:      []AddrDetail{{Addr: "1.2.3.4:9800", LastSeen: "2026-01-01T00:00:00Z"}},
		},
	}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Existing client.
	resp, err := http.Get(srv.URL + "/api/clients/1")
	if err != nil {
		t.Fatalf("GET /api/clients/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var detail ClientDetailInfo
	if err := json.NewDecoder(resp.Body).Decode(&detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.ClientID != 1 {
		t.Errorf("expected clientID 1, got %d", detail.ClientID)
	}
	if len(detail.Addrs) != 1 {
		t.Errorf("expected 1 addr, got %d", len(detail.Addrs))
	}

	// Non-existing client.
	resp2, err := http.Get(srv.URL + "/api/clients/99")
	if err != nil {
		t.Fatalf("GET /api/clients/99: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Errorf("expected 404 for non-existing client, got %d", resp2.StatusCode)
	}
}

func TestServerDeleteClient(t *testing.T) {
	api := &mockServerAPI{
		clients: []ClientInfo{
			{ClientID: 1, VirtualIP: "10.99.0.1"},
		},
	}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Delete existing client.
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/clients/1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /api/clients/1: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if len(api.deletedIDs) != 1 || api.deletedIDs[0] != 1 {
		t.Errorf("expected DeleteClient(1) to be called, got %v", api.deletedIDs)
	}

	// Delete non-existing client.
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/clients/99", nil)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("DELETE /api/clients/99: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 404 {
		t.Errorf("expected 404 for non-existing client, got %d", resp2.StatusCode)
	}
}

func TestServerGetRoutes(t *testing.T) {
	api := &mockServerAPI{
		routes: []RouteEntry{
			{VirtualIP: "10.99.0.1", ClientID: 1},
			{VirtualIP: "10.99.0.2", ClientID: 2},
		},
	}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/routes")
	if err != nil {
		t.Fatalf("GET /api/routes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var routes []RouteEntry
	if err := json.NewDecoder(resp.Body).Decode(&routes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(routes) != 2 {
		t.Errorf("expected 2 routes, got %d", len(routes))
	}
}

// --- Client mode tests ---

func TestClientGetStatus(t *testing.T) {
	api := &mockClientAPI{
		status: StatusInfo{
			Connected: true,
			VirtualIP: "10.99.0.1",
			ClientID:  1,
			SendMode:  "redundant",
		},
	}
	handler := NewHandler("client", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatalf("GET /api/status: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var status StatusInfo
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !status.Connected {
		t.Errorf("expected connected=true")
	}
	if status.VirtualIP != "10.99.0.1" {
		t.Errorf("expected virtualIP 10.99.0.1, got %s", status.VirtualIP)
	}
	if status.SendMode != "redundant" {
		t.Errorf("expected sendMode redundant, got %s", status.SendMode)
	}
}

func TestClientGetInterfaces(t *testing.T) {
	api := &mockClientAPI{
		interfaces: []InterfaceStatus{
			{Name: "eth0", LocalIP: "192.168.1.2", Status: "active", RTT: "12ms", IsActive: true},
			{Name: "wwan0", LocalIP: "10.0.0.1", Status: "suspect", RTT: "45ms", IsActive: false},
		},
	}
	handler := NewHandler("client", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/interfaces")
	if err != nil {
		t.Fatalf("GET /api/interfaces: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var ifaces []InterfaceStatus
	if err := json.NewDecoder(resp.Body).Decode(&ifaces); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("expected 2 interfaces, got %d", len(ifaces))
	}
}

func TestClientSetSendMode(t *testing.T) {
	api := &mockClientAPI{sendMode: "redundant"}
	handler := NewHandler("client", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Valid mode change.
	body := strings.NewReader(`{"mode":"failover"}`)
	resp, err := http.Post(srv.URL+"/api/sendmode", "application/json", body)
	if err != nil {
		t.Fatalf("POST /api/sendmode: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if api.sendMode != "failover" {
		t.Errorf("expected sendMode to be 'failover', got %q", api.sendMode)
	}

	// Invalid mode.
	body2 := strings.NewReader(`{"mode":"invalid"}`)
	resp2, err := http.Post(srv.URL+"/api/sendmode", "application/json", body2)
	if err != nil {
		t.Fatalf("POST /api/sendmode: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Errorf("expected 400 for invalid mode, got %d", resp2.StatusCode)
	}
}

func TestClientInterfaceEnableDisable(t *testing.T) {
	api := &mockClientAPI{}
	handler := NewHandler("client", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Disable.
	resp, err := http.Post(srv.URL+"/api/interfaces/eth0/disable", "", nil)
	if err != nil {
		t.Fatalf("POST disable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if api.lastIface != "eth0" || api.lastEnable != false {
		t.Errorf("expected SetInterfaceEnabled(eth0, false), got (%q, %v)", api.lastIface, api.lastEnable)
	}

	// Enable.
	resp2, err := http.Post(srv.URL+"/api/interfaces/wwan0/enable", "", nil)
	if err != nil {
		t.Fatalf("POST enable: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp2.StatusCode)
	}
	if api.lastIface != "wwan0" || api.lastEnable != true {
		t.Errorf("expected SetInterfaceEnabled(wwan0, true), got (%q, %v)", api.lastIface, api.lastEnable)
	}

	// Invalid action.
	resp3, err := http.Post(srv.URL+"/api/interfaces/eth0/restart", "", nil)
	if err != nil {
		t.Fatalf("POST restart: %v", err)
	}
	defer resp3.Body.Close()
	if resp3.StatusCode != 400 {
		t.Errorf("expected 400 for invalid action, got %d", resp3.StatusCode)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	api := &mockServerAPI{}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// POST to GET-only endpoint.
	resp, err := http.Post(srv.URL+"/api/clients", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/clients: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestStaticFileServing(t *testing.T) {
	api := &mockServerAPI{}
	handler := NewHandler("server", api)
	srv := httptest.NewServer(handler)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected text/html content type, got %q", ct)
	}
}
