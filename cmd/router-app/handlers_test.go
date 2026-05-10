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
		t.Errorf("err = %v, want contains 'too large'", err)
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

func TestFetchURL_AcceptsExactlyAtLimit(t *testing.T) {
	h, _ := newTestHandler(t)
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(bytes.Repeat([]byte{'x'}, subscriptionMaxBody))
	}))
	defer source.Close()
	data, err := h.fetchURL(source.URL)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != subscriptionMaxBody {
		t.Errorf("len = %d, want %d", len(data), subscriptionMaxBody)
	}
}

// --- toggle -------------------------------------------------------------

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
	// The Python fix: toggle OFF must work even when no server is selected,
	// otherwise the user can't get out of a half-broken state.
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
	h.state.SetServers([]vless.Server{{
		ID: "x.com:443", Host: "x.com", Port: 443, UUID: "u",
		Params: map[string]string{"security": "reality", "type": "tcp"},
	}}, "", "")
	h.state.Update(func(d *state.Data) { d.ActiveServerID = "x.com:443" })

	status, body := doJSON(t, "POST", srv.URL+"/api/toggle", map[string]any{"on": true})
	if status != 200 {
		t.Errorf("status = %d, want 200; body = %v", status, body)
	}
	if v, _ := body["vpn_on"].(bool); !v {
		t.Errorf("vpn_on = false, want true")
	}
}

// --- subscription auto-stop --------------------------------------------

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

func TestSubscription_AutoStopsVPNWhenActiveGoesAway(t *testing.T) {
	h, srv := newTestHandler(t)
	h.state.SetServers([]vless.Server{{
		ID: "stale.com:443", Host: "stale.com", Port: 443, UUID: "u",
	}}, "", "")
	h.state.Update(func(d *state.Data) {
		d.ActiveServerID = "stale.com:443"
		d.VPNOn = true
	})

	source := subscriptionSource(t, sampleVLESSBody)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscription",
		map[string]any{"url": source.URL})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on = true, want false (auto-stop)")
	}
	if asString(body["active_server_id"]) != "" {
		t.Errorf("active_server_id = %q, want empty", body["active_server_id"])
	}
	servers, _ := body["servers"].([]any)
	if len(servers) != 2 {
		t.Errorf("got %d servers, want 2", len(servers))
	}
}

func TestSubscription_KeepsActiveIfStillInList(t *testing.T) {
	h, srv := newTestHandler(t)
	h.state.SetServers([]vless.Server{{
		ID: "host1.com:443", Host: "host1.com", Port: 443, UUID: "u",
	}}, "", "")
	h.state.Update(func(d *state.Data) {
		d.ActiveServerID = "host1.com:443"
		d.VPNOn = true
	})

	source := subscriptionSource(t, sampleVLESSBody)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscription",
		map[string]any{"url": source.URL})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if v, _ := body["vpn_on"].(bool); !v {
		t.Errorf("vpn_on = false, want true (kept on)")
	}
	if asString(body["active_server_id"]) != "host1.com:443" {
		t.Errorf("active_server_id = %v", body["active_server_id"])
	}
}

func TestSubscription_NoActiveDoesNotDisturbVPN(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, sampleVLESSBody)
	status, body := doJSON(t, "POST", srv.URL+"/api/subscription",
		map[string]any{"url": source.URL})
	if status != 200 {
		t.Fatalf("status = %d; body = %v", status, body)
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on = true, want false")
	}
}

func TestSubscription_FetchFailureReturns502(t *testing.T) {
	_, srv := newTestHandler(t)
	// Source that closes connections without responding → fetch fails.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, _ := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		conn.Close()
	}))
	defer bad.Close()
	status, body := doJSON(t, "POST", srv.URL+"/api/subscription",
		map[string]any{"url": bad.URL})
	if status != 502 {
		t.Errorf("status = %d, want 502; body = %v", status, body)
	}
}

func TestSubscription_UnparseableReturns400(t *testing.T) {
	_, srv := newTestHandler(t)
	source := subscriptionSource(t, "not a vless list")
	status, body := doJSON(t, "POST", srv.URL+"/api/subscription",
		map[string]any{"url": source.URL})
	if status != 400 {
		t.Errorf("status = %d, want 400; body = %v", status, body)
	}
}

// --- smoke --------------------------------------------------------------

func TestGetState_ReturnsDefaults(t *testing.T) {
	_, srv := newTestHandler(t)
	status, body := doJSON(t, "GET", srv.URL+"/api/state", nil)
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	servers, _ := body["servers"].([]any)
	if len(servers) != 0 {
		t.Errorf("servers = %v, want empty", servers)
	}
	if v, _ := body["vpn_on"].(bool); v {
		t.Errorf("vpn_on = true, want false")
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
