package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/voledyaev/yonder/internal/vless"
)

func newTempState(t *testing.T) (*State, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, path
}

func TestDefaultsWhenNoFile(t *testing.T) {
	s, _ := newTempState(t)
	snap := s.Snapshot()
	if snap.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", snap.Version, SchemaVersion)
	}
	if len(snap.Servers) != 0 {
		t.Errorf("servers = %v, want empty", snap.Servers)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on = true, want false")
	}
	if snap.ActiveServerID != "" {
		t.Errorf("active_server_id = %q, want empty", snap.ActiveServerID)
	}
}

func TestUpdatePersistsAcrossReload(t *testing.T) {
	s, path := newTempState(t)
	if _, err := s.Update(func(d *Data) {
		d.VPNOn = true
		d.ActiveServerID = "x.com:443"
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := s2.Snapshot()
	if !snap.VPNOn {
		t.Errorf("vpn_on did not persist")
	}
	if snap.ActiveServerID != "x.com:443" {
		t.Errorf("active_server_id = %q", snap.ActiveServerID)
	}
}

func TestSetServersClearsStaleActive(t *testing.T) {
	s, _ := newTempState(t)
	s.Update(func(d *Data) { d.ActiveServerID = "old.com:443" })
	s.SetServers([]vless.Server{
		{ID: "new1.com:443"},
		{ID: "new2.com:443"},
	}, "", "")
	if id := s.Snapshot().ActiveServerID; id != "" {
		t.Errorf("active_server_id = %q, want cleared", id)
	}
}

func TestSetServersKeepsActiveIfStillPresent(t *testing.T) {
	s, _ := newTempState(t)
	s.Update(func(d *Data) { d.ActiveServerID = "keep.com:443" })
	s.SetServers([]vless.Server{
		{ID: "other.com:443"},
		{ID: "keep.com:443"},
	}, "", "")
	if id := s.Snapshot().ActiveServerID; id != "keep.com:443" {
		t.Errorf("active_server_id = %q, want keep.com:443", id)
	}
}

func TestCorruptJSONFallsBackToDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New should not fail on corrupt JSON: %v", err)
	}
	snap := s.Snapshot()
	if len(snap.Servers) != 0 {
		t.Errorf("servers = %v, want empty", snap.Servers)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on = true, want false")
	}
}

func TestAtomicNoTmpLeftBehind(t *testing.T) {
	s, path := newTempState(t)
	s.Update(func(d *Data) { d.VPNOn = true })
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not exist after successful write")
	}
}

func TestActiveServerReturnsCopy(t *testing.T) {
	s, _ := newTempState(t)
	s.SetServers([]vless.Server{{ID: "x.com:443", Host: "x.com"}}, "", "")
	s.Update(func(d *Data) { d.ActiveServerID = "x.com:443" })
	active := s.ActiveServer()
	if active == nil {
		t.Fatalf("ActiveServer = nil, want non-nil")
	}
	if active.ID != "x.com:443" {
		t.Errorf("ID = %q", active.ID)
	}
}

func TestActiveServerReturnsNilWhenUnset(t *testing.T) {
	s, _ := newTempState(t)
	if s.ActiveServer() != nil {
		t.Errorf("ActiveServer should be nil when nothing is selected")
	}
}

func TestActiveServerReturnsNilWhenIDUnknown(t *testing.T) {
	s, _ := newTempState(t)
	s.SetServers([]vless.Server{{ID: "x.com:443"}}, "", "")
	s.Update(func(d *Data) { d.ActiveServerID = "notpresent.com:443" })
	if s.ActiveServer() != nil {
		t.Errorf("ActiveServer should be nil when active id is unknown")
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	s, _ := newTempState(t)
	s.SetServers([]vless.Server{{ID: "x.com:443"}}, "", "")
	snap := s.Snapshot()
	snap.Servers = append(snap.Servers, vless.Server{ID: "injected.com:443"})
	if n := len(s.Snapshot().Servers); n != 1 {
		t.Errorf("stored servers = %d, want 1 — snapshot mutation leaked", n)
	}
}

func TestRulesAreRawJSON(t *testing.T) {
	// Rules round-trip as RawMessage so user-supplied JSON structure
	// survives save → reload without re-shaping.
	s, path := newTempState(t)
	rule := json.RawMessage(`{"outboundTag":"proxy","domain":["foo.com"],"type":"field"}`)
	s.Update(func(d *Data) { d.Rules = []json.RawMessage{rule} })

	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	snap := reloaded.Snapshot()
	if len(snap.Rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(snap.Rules))
	}
	var got map[string]any
	if err := json.Unmarshal(snap.Rules[0], &got); err != nil {
		t.Fatalf("rule is not valid JSON after reload: %v", err)
	}
	if got["outboundTag"] != "proxy" {
		t.Errorf("outboundTag = %v", got["outboundTag"])
	}
	domain, _ := got["domain"].([]any)
	if len(domain) != 1 || domain[0] != "foo.com" {
		t.Errorf("domain = %v", got["domain"])
	}
}
