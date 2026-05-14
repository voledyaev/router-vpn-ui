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

// addSub is a tiny helper for tests — pushes a subscription with the given
// servers and returns its generated ID.
func addSub(t *testing.T, s *State, label, source string, servers []vless.Server) string {
	t.Helper()
	if _, err := s.AddSubscription(label, source, servers); err != nil {
		t.Fatalf("AddSubscription: %v", err)
	}
	subs := s.Snapshot().Subscriptions
	return subs[len(subs)-1].ID
}

func TestDefaultsWhenNoFile(t *testing.T) {
	s, _ := newTempState(t)
	snap := s.Snapshot()
	if snap.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", snap.Version, SchemaVersion)
	}
	if len(snap.Subscriptions) != 0 {
		t.Errorf("subscriptions = %v, want empty", snap.Subscriptions)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on = true, want false")
	}
	if snap.ActiveServer != nil {
		t.Errorf("active_server = %+v, want nil", snap.ActiveServer)
	}
}

func TestAddSubscription_AppendsAndAssignsID(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "https://foo.example/sub",
		[]vless.Server{{ID: "a:443"}})
	if id == "" {
		t.Errorf("generated id is empty")
	}
	subs := s.Snapshot().Subscriptions
	if len(subs) != 1 {
		t.Fatalf("got %d subs, want 1", len(subs))
	}
	if subs[0].Label != "Foo" {
		t.Errorf("label = %q", subs[0].Label)
	}
	if subs[0].Source != "https://foo.example/sub" {
		t.Errorf("source = %q", subs[0].Source)
	}
	if subs[0].FetchedAt == "" {
		t.Errorf("fetched_at is empty")
	}
}

func TestAddSubscription_AllowsDuplicateSource(t *testing.T) {
	// User may want two cards for the same URL (e.g. testing label changes).
	s, _ := newTempState(t)
	addSub(t, s, "First", "https://foo.example/sub", nil)
	addSub(t, s, "Second", "https://foo.example/sub", nil)
	if n := len(s.Snapshot().Subscriptions); n != 2 {
		t.Errorf("got %d subs, want 2", n)
	}
}

func TestPersistAcrossReload(t *testing.T) {
	s, path := newTempState(t)
	id := addSub(t, s, "Foo", "https://x", []vless.Server{{ID: "h:443"}})
	if _, err := s.Update(func(d *Data) {
		d.VPNOn = true
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id, ServerID: "h:443"}
	}); err != nil {
		t.Fatal(err)
	}
	s2, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := s2.Snapshot()
	if !snap.VPNOn {
		t.Errorf("vpn_on did not persist")
	}
	if snap.ActiveServer == nil || snap.ActiveServer.SubscriptionID != id {
		t.Errorf("active_server = %+v", snap.ActiveServer)
	}
	if len(snap.Subscriptions) != 1 || snap.Subscriptions[0].ID != id {
		t.Errorf("subscription did not persist")
	}
}

func TestDeleteSubscription_ClearsActiveWhenAffected(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x", []vless.Server{{ID: "h:443"}})
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id, ServerID: "h:443"}
		d.VPNOn = true
	})
	if _, err := s.DeleteSubscription(id); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.ActiveServer != nil {
		t.Errorf("active_server = %+v, want nil after deletion", snap.ActiveServer)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on = true, want false after losing active server")
	}
	if len(snap.Subscriptions) != 0 {
		t.Errorf("subscriptions = %v", snap.Subscriptions)
	}
}

func TestDeleteSubscription_KeepsActiveWhenUnrelated(t *testing.T) {
	s, _ := newTempState(t)
	id1 := addSub(t, s, "A", "x", []vless.Server{{ID: "h:443"}})
	id2 := addSub(t, s, "B", "y", []vless.Server{{ID: "k:443"}})
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id1, ServerID: "h:443"}
		d.VPNOn = true
	})
	if _, err := s.DeleteSubscription(id2); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.ActiveServer == nil || snap.ActiveServer.SubscriptionID != id1 {
		t.Errorf("active_server = %+v, want subscription %q", snap.ActiveServer, id1)
	}
	if !snap.VPNOn {
		t.Errorf("vpn_on = false, want true (untouched)")
	}
}

func TestReplaceSubscriptionServers_ClearsActiveWhenServerGone(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x", []vless.Server{{ID: "old:443"}})
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id, ServerID: "old:443"}
		d.VPNOn = true
	})
	if _, err := s.ReplaceSubscriptionServers(id,
		[]vless.Server{{ID: "new:443"}}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.ActiveServer != nil {
		t.Errorf("active_server = %+v, want cleared", snap.ActiveServer)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on = true, want false")
	}
}

func TestReplaceSubscriptionServers_KeepsActiveWhenStillPresent(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x", []vless.Server{{ID: "stay:443"}})
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id, ServerID: "stay:443"}
		d.VPNOn = true
	})
	if _, err := s.ReplaceSubscriptionServers(id, []vless.Server{
		{ID: "other:443"},
		{ID: "stay:443"},
	}); err != nil {
		t.Fatal(err)
	}
	snap := s.Snapshot()
	if snap.ActiveServer == nil || snap.ActiveServer.ServerID != "stay:443" {
		t.Errorf("active_server = %+v", snap.ActiveServer)
	}
	if !snap.VPNOn {
		t.Errorf("vpn_on flipped off unexpectedly")
	}
}

func TestRenameSubscription(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Old", "x", nil)
	if _, err := s.RenameSubscription(id, "New"); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Subscriptions[0].Label != "New" {
		t.Errorf("label not renamed")
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
	if len(snap.Subscriptions) != 0 {
		t.Errorf("subscriptions = %v, want empty", snap.Subscriptions)
	}
}

func TestVersionMismatchFallsBackToDefaults(t *testing.T) {
	// v1-style state.json with `subscription_url` + flat servers is rejected
	// at load. User must re-enter subscriptions through the UI.
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte(`{
		"version": 1,
		"subscription_url": "https://old.example/sub",
		"servers": [{"id": "x:443"}],
		"active_server_id": "x:443",
		"vpn_on": true
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	snap := s.Snapshot()
	if snap.Version != SchemaVersion {
		t.Errorf("version = %d, want %d", snap.Version, SchemaVersion)
	}
	if len(snap.Subscriptions) != 0 {
		t.Errorf("subscriptions = %v, want empty (v1 state should not carry over)", snap.Subscriptions)
	}
	if snap.VPNOn {
		t.Errorf("vpn_on leaked from v1 state — must reset")
	}
}

func TestAtomicNoTmpLeftBehind(t *testing.T) {
	s, path := newTempState(t)
	s.Update(func(d *Data) { d.VPNOn = true })
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp file should not exist after successful write")
	}
}

func TestActiveServer_ResolvesToCopy(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x",
		[]vless.Server{{ID: "h:443", Host: "h", Params: map[string]string{"k": "v"}}})
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: id, ServerID: "h:443"}
	})
	active := s.ActiveServer()
	if active == nil {
		t.Fatalf("ActiveServer = nil")
	}
	if active.ID != "h:443" || active.Host != "h" {
		t.Errorf("active = %+v", active)
	}
	// Mutating the returned copy must not affect stored state.
	active.Params["k"] = "mutated"
	if v := s.ActiveServer().Params["k"]; v != "v" {
		t.Errorf("stored params were mutated through ActiveServer copy: %q", v)
	}
}

func TestActiveServer_NilWhenUnset(t *testing.T) {
	s, _ := newTempState(t)
	if s.ActiveServer() != nil {
		t.Errorf("ActiveServer should be nil when nothing is selected")
	}
}

func TestActiveServer_NilWhenSubscriptionMissing(t *testing.T) {
	s, _ := newTempState(t)
	s.Update(func(d *Data) {
		d.ActiveServer = &ActiveServerRef{SubscriptionID: "ghost", ServerID: "h:443"}
	})
	if s.ActiveServer() != nil {
		t.Errorf("ActiveServer should be nil when subscription does not exist")
	}
}

func TestSnapshotIsIndependentCopy(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x", []vless.Server{{ID: "h:443"}})
	_ = id
	snap := s.Snapshot()
	snap.Subscriptions[0].Servers = append(snap.Subscriptions[0].Servers,
		vless.Server{ID: "injected:443"})
	if n := len(s.Snapshot().Subscriptions[0].Servers); n != 1 {
		t.Errorf("stored servers = %d, want 1 — snapshot mutation leaked", n)
	}
}

func TestRulesAreRawJSON(t *testing.T) {
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

func TestHasServer(t *testing.T) {
	s, _ := newTempState(t)
	id := addSub(t, s, "Foo", "x", []vless.Server{{ID: "h:443"}})
	if !s.HasServer(id, "h:443") {
		t.Errorf("HasServer(known) = false")
	}
	if s.HasServer(id, "ghost:443") {
		t.Errorf("HasServer(ghost server) = true")
	}
	if s.HasServer("ghost-sub", "h:443") {
		t.Errorf("HasServer(ghost sub) = true")
	}
}
