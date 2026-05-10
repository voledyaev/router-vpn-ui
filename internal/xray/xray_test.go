package xray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voledyaev/yonder/internal/vless"
)

var realityServer = vless.Server{
	ID:   "host.com:443",
	Host: "host.com",
	Port: 443,
	UUID: "uuid-1234",
	Params: map[string]string{
		"security": "reality",
		"type":     "tcp",
		"flow":     "xtls-rprx-vision",
		"sni":      "example.com",
		"fp":       "chrome",
		"pbk":      "PUB",
		"sid":      "SID",
	},
}

// --- BuildOutbound ------------------------------------------------------

func TestBuildOutbound_RealityShape(t *testing.T) {
	ob := BuildOutbound(realityServer)
	if ob["protocol"] != "vless" {
		t.Errorf("protocol = %v, want vless", ob["protocol"])
	}
	stream := ob["streamSettings"].(map[string]any)
	if stream["network"] != "tcp" {
		t.Errorf("network = %v", stream["network"])
	}
	if stream["security"] != "reality" {
		t.Errorf("security = %v", stream["security"])
	}
	rs := stream["realitySettings"].(map[string]any)
	if rs["serverName"] != "example.com" {
		t.Errorf("serverName = %v", rs["serverName"])
	}
	if rs["fingerprint"] != "chrome" {
		t.Errorf("fingerprint = %v", rs["fingerprint"])
	}
	if rs["publicKey"] != "PUB" {
		t.Errorf("publicKey = %v", rs["publicKey"])
	}
	if rs["shortId"] != "SID" {
		t.Errorf("shortId = %v", rs["shortId"])
	}
	if rs["spiderX"] != "" {
		t.Errorf("spiderX = %v, want empty (no spx in link)", rs["spiderX"])
	}

	users := ob["settings"].(map[string]any)["vnext"].([]map[string]any)[0]["users"].([]map[string]any)
	user := users[0]
	if user["id"] != "uuid-1234" {
		t.Errorf("user id = %v", user["id"])
	}
	if user["flow"] != "xtls-rprx-vision" {
		t.Errorf("flow = %v", user["flow"])
	}
	if user["encryption"] != "none" {
		t.Errorf("encryption = %v", user["encryption"])
	}
}

func TestBuildOutbound_RealitySpxForwarded(t *testing.T) {
	// Providers may set spx=/path to control Reality's post-handshake
	// disguise GET; the parser must pass it through.
	srv := realityServer
	srv.Params = copyParams(realityServer.Params)
	srv.Params["spx"] = "/"
	ob := BuildOutbound(srv)
	rs := ob["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
	if rs["spiderX"] != "/" {
		t.Errorf("spiderX = %v, want /", rs["spiderX"])
	}
}

func TestBuildOutbound_WSTransport(t *testing.T) {
	srv := vless.Server{
		Host: "ws.example.com", Port: 443, UUID: "u",
		Params: map[string]string{
			"security": "tls",
			"type":     "ws",
			"path":     "/wspath",
			"host":     "ws.example.com",
			"sni":      "ws.example.com",
		},
	}
	ob := BuildOutbound(srv)
	stream := ob["streamSettings"].(map[string]any)
	if stream["network"] != "ws" {
		t.Errorf("network = %v", stream["network"])
	}
	if stream["security"] != "tls" {
		t.Errorf("security = %v", stream["security"])
	}
	ws := stream["wsSettings"].(map[string]any)
	if ws["path"] != "/wspath" {
		t.Errorf("ws path = %v", ws["path"])
	}
	headers := ws["headers"].(map[string]string)
	if headers["Host"] != "ws.example.com" {
		t.Errorf("ws Host header = %v", headers["Host"])
	}
	tls := stream["tlsSettings"].(map[string]any)
	if tls["serverName"] != "ws.example.com" {
		t.Errorf("tls serverName = %v", tls["serverName"])
	}
}

func TestBuildOutbound_GRPCTransport(t *testing.T) {
	srv := vless.Server{
		Host: "g.example.com", Port: 443, UUID: "u",
		Params: map[string]string{
			"security": "tls",
			"type":     "grpc",
			"path":     "/grpcservice",
			"sni":      "g.example.com",
		},
	}
	ob := BuildOutbound(srv)
	stream := ob["streamSettings"].(map[string]any)
	if stream["network"] != "grpc" {
		t.Errorf("network = %v", stream["network"])
	}
	grpc := stream["grpcSettings"].(map[string]any)
	if grpc["serviceName"] != "grpcservice" {
		t.Errorf("serviceName = %v, want grpcservice (leading / stripped)", grpc["serviceName"])
	}
}

// --- WriteXKeenSplit ----------------------------------------------------

func TestWriteXKeenSplit_BothFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteXKeenSplit(&realityServer, nil, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	for _, name := range []string{OutboundsFile, RoutingFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestWriteXKeenSplit_OutboundsContent(t *testing.T) {
	dir := t.TempDir()
	if err := WriteXKeenSplit(&realityServer, nil, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	tags := readOutboundTags(t, filepath.Join(dir, OutboundsFile))
	want := []string{"proxy", "direct", "block"}
	if !equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestWriteXKeenSplit_NoServerOmitsProxy(t *testing.T) {
	dir := t.TempDir()
	if err := WriteXKeenSplit(nil, nil, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	tags := readOutboundTags(t, filepath.Join(dir, OutboundsFile))
	want := []string{"direct", "block"}
	if !equal(tags, want) {
		t.Errorf("tags = %v, want %v", tags, want)
	}
}

func TestWriteXKeenSplit_CustomRules(t *testing.T) {
	dir := t.TempDir()
	custom := []json.RawMessage{
		json.RawMessage(`{"type":"field","outboundTag":"proxy","domain":["foo.com"]}`),
	}
	if err := WriteXKeenSplit(&realityServer, custom, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, RoutingFile))
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal routing: %v", err)
	}
	routing := doc["routing"].(map[string]any)
	if routing["domainStrategy"] != "AsIs" {
		t.Errorf("domainStrategy = %v", routing["domainStrategy"])
	}
	if !strings.Contains(string(raw), `"foo.com"`) {
		t.Errorf("custom rule not preserved:\n%s", raw)
	}
}

func TestWriteXKeenSplit_DefaultRulesIncludeRFC1918(t *testing.T) {
	dir := t.TempDir()
	if err := WriteXKeenSplit(&realityServer, nil, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, RoutingFile))
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
		"127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10"} {
		if !strings.Contains(string(raw), cidr) {
			t.Errorf("default rules missing %s", cidr)
		}
	}
}

func TestWriteXKeenSplit_AtomicNoTmpLeftBehind(t *testing.T) {
	dir := t.TempDir()
	if err := WriteXKeenSplit(&realityServer, nil, dir); err != nil {
		t.Fatalf("WriteXKeenSplit: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("leftover .tmp file: %s", e.Name())
		}
	}
}

// --- helpers ------------------------------------------------------------

func readOutboundTags(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Outbounds []struct {
			Tag string `json:"tag"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	tags := make([]string, len(doc.Outbounds))
	for i, o := range doc.Outbounds {
		tags[i] = o.Tag
	}
	return tags
}

func copyParams(p map[string]string) map[string]string {
	out := make(map[string]string, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
