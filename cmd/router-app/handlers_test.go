package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voledyaev/yonder/internal/state"
	"github.com/voledyaev/yonder/internal/vless"
)

const sampleVLESSBody = "vless://uuid1@host1.com:443?security=reality&type=tcp" +
	"#%F0%9F%87%B5%F0%9F%87%B1Poland\n" +
	"vless://uuid2@host2.com:443?security=reality&type=tcp" +
	"#%F0%9F%87%A9%F0%9F%87%AAGermany\n"

// newTestHandler returns a Handler wired to a temp state file and temp xray
// configs dir, plus an httptest server fronting it. xkeen is not installed
// on the test host, so services.* calls become no-op skips.
func newTestHandler(t *testing.T) (*Handler, *httptest.Server) {
	t.Helper()
	dir := t.TempDir()

	st, err := state.New(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	staticSub, _ := fs.Sub(staticFS, "static")

	h := &Handler{
		state:          st,
		xrayConfigsDir: filepath.Join(dir, "configs"),
		logger:         log.New(os.Stderr, "test ", 0),
		httpClient:     &http.Client{Timeout: 3 * time.Second},
		staticFS:       staticSub,
	}
	mux := http.NewServeMux()
	h.register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return h, srv
}

func doJSON(t *testing.T, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reqBody = bytes.NewReader(raw)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reqBody)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// subscriptionSource spins up an httptest server that serves the canned
// VLESS body to any GET, so the handler's fetchURL has somewhere to hit.
func subscriptionSource(t *testing.T, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

// helpers to drill into the snapshot map returned by handler endpoints
func subscriptions(body map[string]any) []any {
	subs, _ := body["subscriptions"].([]any)
	return subs
}

func subAt(body map[string]any, i int) map[string]any {
	subs := subscriptions(body)
	if i >= len(subs) {
		return nil
	}
	m, _ := subs[i].(map[string]any)
	return m
}

// --- fetchURL size limit ------------------------------------------------

func TestFetchURL_RejectsOversize(t *testing.T) {
	h, _ := newTestHandler(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte{'x'}, subscriptionMaxBody+100))
	}))
	defer source.Close()
	_, err := h.fetchURL(source.URL)
	if err == nil {
		t.Fatal("expected error for oversize body")
	}
	if !strings.Contains(err.Error(), "too large") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchURL_AcceptsUnderLimit(t *testing.T) {
	h, _ := newTestHandler(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("hello world"))
	}))
	defer source.Close()
	data, err := h.fetchURL(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello world" {
		t.Errorf("data = %q", data)
	}
}

// --- POST /api/subscriptions --------------------------------------------

func TestAddSubscription_HappyPath(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "Test", "source": source.URL})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	subs := subscriptions(body)
	if len(subs) != 1 {
		t.Fatalf("got %d subs, want 1", len(subs))
	}
	sub := subAt(body, 0)
	if sub["label"] != "Test" {
		t.Errorf("label = %v", sub["label"])
	}
	servers, _ := sub["servers"].([]any)
	if len(servers) != 2 {
		t.Errorf("servers = %d, want 2", len(servers))
	}
}

func TestAddSubscription_InlineVless(t *testing.T) {
	_, srv := newTestHandler(t)
	// Source starts with vless:// — handler should skip HTTP fetch entirely
	// and parse the URI in place.
	inline := "vless://abc@host.example:8443?security=reality&type=tcp#test"
	status, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "Inline", "source": inline})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	sub := subAt(body, 0)
	if sub["source"] != inline {
		t.Errorf("stored source = %v", sub["source"])
	}
	servers, _ := sub["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	srv0, _ := servers[0].(map[string]any)
	if srv0["host"] != "host.example" {
		t.Errorf("host = %v", srv0["host"])
	}
}

func TestAddSubscription_LabelOptional_DerivedFromHost(t *testing.T) {
	// Empty label is allowed — backend derives a label from the source.
	// For URL sources we use the hostname; subscription fetch must still
	// succeed for the request to land.
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"source": source.URL})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	label, _ := subAt(body, 0)["label"].(string)
	// httptest hostname is 127.0.0.1:<port> — we don't pin the exact value,
	// just that derivation kicked in (non-empty, contains a colon).
	if label == "" || !strings.Contains(label, ":") {
		t.Errorf("label = %q, want auto-derived host:port from source", label)
	}
}

func TestAddSubscription_LabelOptional_VlessHost(t *testing.T) {
	// Inline vless:// source — derived label should be the proxy host
	// (between @ and the next : / ? / #), NOT the link's UUID.
	_, srv := newTestHandler(t)
	inline := "vless://abc@host.example:8443?security=reality&type=tcp#x"
	status, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"source": inline})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if got, _ := subAt(body, 0)["label"].(string); got != "host.example" {
		t.Errorf("label = %q, want %q", got, "host.example")
	}
}

func TestAddSubscription_BadSourceScheme(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": "ftp://example.com/x"})
	if status != 400 {
		t.Errorf("status = %d, want 400; body = %v", status, body)
	}
}

func TestAddSubscription_FetchFailure(t *testing.T) {
	_, srv := newTestHandler(t)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer bad.Close()
	status, _ := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": bad.URL})
	if status != 502 {
		t.Errorf("status = %d, want 502", status)
	}
}

func TestAddSubscription_UnparseableReturns400(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, "not a vless list")
	status, _ := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": source.URL})
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
}

// --- DELETE /api/subscriptions/{id} -------------------------------------

func TestDeleteSubscription_RemovesAndClearsActive(t *testing.T) {
	h, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	_, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": source.URL})
	subID := subAt(body, 0)["id"].(string)

	// Activate a server in this subscription and turn VPN on.
	srvID := subAt(body, 0)["servers"].([]any)[0].(map[string]any)["id"].(string)
	doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": subID, "server_id": srvID})
	doJSON(t, "POST", srv.URL+"/api/toggle", map[string]any{"on": true})

	status, body := doJSON(t, "DELETE", srv.URL+"/api/subscriptions/"+subID, nil)
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if len(subscriptions(body)) != 0 {
		t.Errorf("subscriptions not removed: %v", subscriptions(body))
	}
	if body["active_server"] != nil {
		t.Errorf("active_server should be cleared: %v", body["active_server"])
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on should be false after losing active server")
	}
	_ = h
}

func TestDeleteSubscription_Unknown404(t *testing.T) {
	_, srv := newTestHandler(t)
	status, _ := doJSON(t, "DELETE", srv.URL+"/api/subscriptions/ghost-id", nil)
	if status != 404 {
		t.Errorf("status = %d, want 404", status)
	}
}

// --- POST /api/subscriptions/{id}/refresh -------------------------------

func TestRefreshSubscription_ReplacesServers(t *testing.T) {
	_, srv := newTestHandler(t)
	// First fetch returns one server, second returns two — simulates a
	// provider adding a node.
	calls := 0
	bodies := []string{
		"vless://uuid1@host1.com:443?security=reality#%F0%9F%87%B5%F0%9F%87%B1Poland\n",
		"vless://uuid1@host1.com:443?security=reality#%F0%9F%87%B5%F0%9F%87%B1Poland\n" +
			"vless://uuid2@host2.com:443?security=reality#%F0%9F%87%A9%F0%9F%87%AAGermany\n",
	}
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		i := calls
		calls++
		if i >= len(bodies) {
			i = len(bodies) - 1
		}
		w.Write([]byte(bodies[i]))
	}))
	defer source.Close()

	_, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": source.URL})
	subID := subAt(body, 0)["id"].(string)
	if n := len(subAt(body, 0)["servers"].([]any)); n != 1 {
		t.Fatalf("initial servers = %d, want 1", n)
	}

	status, body := doJSON(t, "POST",
		srv.URL+"/api/subscriptions/"+subID+"/refresh", nil)
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if n := len(subAt(body, 0)["servers"].([]any)); n != 2 {
		t.Errorf("after refresh: servers = %d, want 2", n)
	}
}

// --- PATCH /api/subscriptions/{id} --------------------------------------

func TestPatchSubscription_Rename(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	_, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "Old", "source": source.URL})
	subID := subAt(body, 0)["id"].(string)

	status, body := doJSON(t, "PATCH", srv.URL+"/api/subscriptions/"+subID,
		map[string]any{"label": "New"})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if subAt(body, 0)["label"] != "New" {
		t.Errorf("label = %v", subAt(body, 0)["label"])
	}
}

// --- POST /api/server (composite ref) -----------------------------------

func TestServerSelect_ValidRef(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	_, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": source.URL})
	subID := subAt(body, 0)["id"].(string)
	servers := subAt(body, 0)["servers"].([]any)
	srvID := servers[0].(map[string]any)["id"].(string)

	status, body := doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": subID, "server_id": srvID})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	a, _ := body["active_server"].(map[string]any)
	if a == nil {
		t.Fatalf("active_server is nil")
	}
	if a["subscription_id"] != subID || a["server_id"] != srvID {
		t.Errorf("active_server = %+v", a)
	}
}

func TestServerSelect_InvalidRefRejected(t *testing.T) {
	_, srv := newTestHandler(t)
	status, _ := doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": "ghost", "server_id": "h:443"})
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
}

func TestServerSelect_DeselectsViaNulls(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	_, body := doJSON(t, "POST", srv.URL+"/api/subscriptions",
		map[string]any{"label": "X", "source": source.URL})
	subID := subAt(body, 0)["id"].(string)
	srvID := subAt(body, 0)["servers"].([]any)[0].(map[string]any)["id"].(string)
	doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": subID, "server_id": srvID})

	// null + null → deselect
	status, body := doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": nil, "server_id": nil})
	if status != 200 {
		t.Fatalf("status = %d", status)
	}
	if body["active_server"] != nil {
		t.Errorf("active_server should be cleared: %v", body["active_server"])
	}
}

// --- POST /api/toggle ---------------------------------------------------

func TestToggle_OnWithoutActiveRejected(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "POST", srv.URL+"/api/toggle", map[string]any{"on": true})
	if status != 400 {
		t.Errorf("status = %d, want 400", status)
	}
	if !strings.Contains(asString(body["error"]), "no active server") {
		t.Errorf("error = %v", body["error"])
	}
}

func TestToggle_OffWithoutActiveSucceeds(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "POST", srv.URL+"/api/toggle", map[string]any{"on": false})
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on = true, want false")
	}
}

func TestToggle_OnWithActiveSucceeds(t *testing.T) {
	h, srv := newTestHandler(t)
	_, err := h.state.AddSubscription("X", "vless://uuid@host.com:443?security=reality#x",
		[]vless.Server{{ID: "host.com:443", Host: "host.com", Port: 443, UUID: "uuid"}})
	if err != nil {
		t.Fatal(err)
	}
	subID := h.state.Snapshot().Subscriptions[0].ID
	doJSON(t, "POST", srv.URL+"/api/server",
		map[string]any{"subscription_id": subID, "server_id": "host.com:443"})

	status, body := doJSON(t, "POST", srv.URL+"/api/toggle", map[string]any{"on": true})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if v, _ := body["vpn_on"].(bool); !v {
		t.Errorf("vpn_on = false, want true")
	}
}

// --- smoke --------------------------------------------------------------

func TestGetState_ReturnsDefaults(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "GET", srv.URL+"/api/state", nil)
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if len(subscriptions(body)) != 0 {
		t.Errorf("subscriptions = %v, want empty", subscriptions(body))
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on = true, want false")
	}
	if body["active_server"] != nil {
		t.Errorf("active_server = %v, want nil", body["active_server"])
	}
}

func TestUnknownAPI_Returns404(t *testing.T) {
	_, srv := newTestHandler(t)
	status, _ := doJSON(t, "GET", srv.URL+"/api/nope", nil)
	if status != 404 {
		t.Errorf("status = %d, want 404", status)
	}
}

func TestHealth(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "GET", srv.URL+"/api/health", nil)
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if v, _ := body["ok"].(bool); !v {
		t.Errorf("ok = false")
	}
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
