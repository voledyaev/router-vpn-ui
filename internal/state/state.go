// Package state holds the persistent state of the yonder daemon.
//
// Single JSON file under $RVU_BASE_DIR. Atomic writes via write-temp-then-
// rename so a crash mid-write cannot corrupt saved state.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/voledyaev/yonder/internal/vless"
)

// SchemaVersion v2 introduced multi-subscription support (each subscription
// becomes its own card in the UI). v1 had a single subscription_url + flat
// servers list; the on-disk fields are intentionally not migrated — when
// state.json is from v1 the schema mismatch silently falls back to v2
// defaults, and the user re-enters their subscriptions through the UI.
const SchemaVersion = 2

// Subscription is one source of VLESS servers. Source may be an HTTP(S)
// URL (which yonder fetches), or a literal `vless://...` URI (parsed
// inline; refresh is a no-op).
type Subscription struct {
	ID        string         `json:"id"`
	Label     string         `json:"label"`
	Source    string         `json:"source"`
	FetchedAt string         `json:"fetched_at"`
	Servers   []vless.Server `json:"servers"`
}

// ActiveServerRef points at one specific server inside one specific
// subscription. Composite key because two subscriptions can in principle
// contain the same host:port — making this composite also means deleting a
// subscription cleanly resets the active server when the active selection
// was inside it.
type ActiveServerRef struct {
	SubscriptionID string `json:"subscription_id"`
	ServerID       string `json:"server_id"`
}

// ApplyResult records the outcome of the most recent xkeen apply cycle.
// Persisted so the UI can show a non-transient status — earlier we wiped
// `last_error` on the next successful apply, which made transient failures
// invisible if the user kept clicking.
type ApplyResult struct {
	At  string `json:"at"`  // ISO timestamp
	OK  bool   `json:"ok"`  // false when xkeen failed or timed out
	Msg string `json:"msg"` // user-readable detail; "" on success
}

// Data is the on-disk schema. It is also what Snapshot returns to callers.
type Data struct {
	Version           int               `json:"version"`
	Subscriptions     []Subscription    `json:"subscriptions"`
	ActiveServer      *ActiveServerRef  `json:"active_server"` // nil when nothing selected
	VPNOn             bool              `json:"vpn_on"`
	RulesURL          string            `json:"rules_url"`
	RulesFetchedAt    string            `json:"rules_fetched_at"`
	Rules             []json.RawMessage `json:"rules"`
	RulesWarnings     []string          `json:"rules_warnings"`
	RulesSkippedCount int               `json:"rules_skipped_count"`
	LastError         string            `json:"last_error"` // mirrors LastApply.Msg when LastApply.OK=false
	LastApply         *ApplyResult      `json:"last_apply"` // nil until first apply runs
	Applying          bool              `json:"applying"`   // true between requestApply and the worker finishing its iteration
}

// Defaults returns a fresh Data with safe zero values. Slices are always
// non-nil so JSON marshals them as [] rather than null.
func Defaults() Data {
	return Data{
		Version:       SchemaVersion,
		Subscriptions: []Subscription{},
		Rules:         []json.RawMessage{},
		RulesWarnings: []string{},
	}
}

// State is a thread-safe wrapper around the JSON file.
type State struct {
	path string
	mu   sync.RWMutex
	data Data
}

// New opens the state file at path. If the file is missing, corrupt, or
// from an incompatible schema version, defaults are loaded and no error
// is returned — the next Update will create or overwrite the file.
func New(path string) (*State, error) {
	s := &State{path: path, data: Defaults()}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *State) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var loaded Data
	if err := json.Unmarshal(raw, &loaded); err != nil {
		// Corrupt file: keep defaults, don't crash. The next save will
		// rewrite it cleanly.
		return nil
	}
	// Older schema versions are silently dropped — we don't migrate.
	// (Saved state from v1 has no `subscriptions` field, so a v1 file
	// would surface as "empty" but with vpn_on=true potentially, which
	// is wrong; reset to defaults instead.)
	if loaded.Version != SchemaVersion {
		return nil
	}
	if loaded.Subscriptions == nil {
		loaded.Subscriptions = []Subscription{}
	}
	if loaded.Rules == nil {
		loaded.Rules = []json.RawMessage{}
	}
	if loaded.RulesWarnings == nil {
		loaded.RulesWarnings = []string{}
	}
	s.data = loaded
	return nil
}

// Snapshot returns a copy of the current state safe to expose to callers.
func (s *State) Snapshot() Data {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneData(s.data)
}

// ActiveServer returns a pointer to the currently selected server, or nil
// if nothing is selected or the selection points at a server that no
// longer exists (deleted subscription, refreshed-away server, etc).
func (s *State) ActiveServer() *vless.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.ActiveServer == nil {
		return nil
	}
	ref := *s.data.ActiveServer
	for i := range s.data.Subscriptions {
		if s.data.Subscriptions[i].ID != ref.SubscriptionID {
			continue
		}
		for j := range s.data.Subscriptions[i].Servers {
			if s.data.Subscriptions[i].Servers[j].ID == ref.ServerID {
				srv := s.data.Subscriptions[i].Servers[j]
				if srv.Params != nil {
					p := make(map[string]string, len(srv.Params))
					for k, v := range srv.Params {
						p[k] = v
					}
					srv.Params = p
				}
				return &srv
			}
		}
	}
	return nil
}

// Update applies fn under the write lock, persists, and returns a snapshot.
// Use this for atomic read-modify-write of multiple fields.
func (s *State) Update(fn func(*Data)) (Data, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.data)
	if err := s.saveLocked(); err != nil {
		return Data{}, err
	}
	return cloneData(s.data), nil
}

// AddSubscription appends a new subscription with a freshly-generated ID.
// Returns the new state snapshot. Labels and sources are not deduplicated —
// users may want multiple subscriptions with the same source under
// different labels.
func (s *State) AddSubscription(label, source string, servers []vless.Server) (Data, error) {
	sub := Subscription{
		ID:        newSubscriptionID(),
		Label:     label,
		Source:    source,
		FetchedAt: nowISO(),
		Servers:   servers,
	}
	if sub.Servers == nil {
		sub.Servers = []vless.Server{}
	}
	return s.Update(func(d *Data) {
		d.Subscriptions = append(d.Subscriptions, sub)
	})
}

// DeleteSubscription removes a subscription by ID. If the active server
// was inside the deleted subscription, the active selection is cleared
// and vpn_on is forced off (so the daemon's apply worker stops xkeen
// rather than running it without a target).
func (s *State) DeleteSubscription(id string) (Data, error) {
	return s.Update(func(d *Data) {
		filtered := make([]Subscription, 0, len(d.Subscriptions))
		for _, sub := range d.Subscriptions {
			if sub.ID != id {
				filtered = append(filtered, sub)
			}
		}
		d.Subscriptions = filtered
		if d.ActiveServer != nil && d.ActiveServer.SubscriptionID == id {
			d.ActiveServer = nil
			d.VPNOn = false
		}
	})
}

// ReplaceSubscriptionServers updates a subscription's server list (after a
// refresh or source edit). FetchedAt is bumped. If the active server was
// inside this subscription and is no longer in the new server list, the
// active selection is cleared and vpn_on is forced off.
func (s *State) ReplaceSubscriptionServers(id string, servers []vless.Server) (Data, error) {
	return s.Update(func(d *Data) {
		for i := range d.Subscriptions {
			if d.Subscriptions[i].ID != id {
				continue
			}
			if servers == nil {
				servers = []vless.Server{}
			}
			d.Subscriptions[i].Servers = servers
			d.Subscriptions[i].FetchedAt = nowISO()
			break
		}
		if d.ActiveServer == nil || d.ActiveServer.SubscriptionID != id {
			return
		}
		stillPresent := false
		for _, srv := range servers {
			if srv.ID == d.ActiveServer.ServerID {
				stillPresent = true
				break
			}
		}
		if !stillPresent {
			d.ActiveServer = nil
			d.VPNOn = false
		}
	})
}

// RenameSubscription changes a subscription's user-facing label.
func (s *State) RenameSubscription(id, label string) (Data, error) {
	return s.Update(func(d *Data) {
		for i := range d.Subscriptions {
			if d.Subscriptions[i].ID == id {
				d.Subscriptions[i].Label = label
				return
			}
		}
	})
}

// HasSubscription reports whether a subscription with this ID exists.
// Used by handlers to validate input before applying mutations.
func (s *State) HasSubscription(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.data.Subscriptions {
		if sub.ID == id {
			return true
		}
	}
	return false
}

// HasServer reports whether the given (subscription_id, server_id) pair
// refers to a known server. Used by handlers to validate active-server
// selection.
func (s *State) HasServer(subscriptionID, serverID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, sub := range s.data.Subscriptions {
		if sub.ID != subscriptionID {
			continue
		}
		for _, srv := range sub.Servers {
			if srv.ID == serverID {
				return true
			}
		}
		return false
	}
	return false
}

func (s *State) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// cloneData returns a deep-enough copy: slices and maps are recreated, so
// callers can mutate the result without affecting stored state.
func cloneData(d Data) Data {
	out := d
	if d.ActiveServer != nil {
		ref := *d.ActiveServer
		out.ActiveServer = &ref
	}
	if d.LastApply != nil {
		r := *d.LastApply
		out.LastApply = &r
	}
	if d.Subscriptions != nil {
		out.Subscriptions = make([]Subscription, len(d.Subscriptions))
		for i, sub := range d.Subscriptions {
			out.Subscriptions[i] = sub
			out.Subscriptions[i].Servers = cloneServers(sub.Servers)
		}
	} else {
		out.Subscriptions = []Subscription{}
	}
	if d.Rules != nil {
		out.Rules = make([]json.RawMessage, len(d.Rules))
		for i, r := range d.Rules {
			b := make([]byte, len(r))
			copy(b, r)
			out.Rules[i] = b
		}
	} else {
		out.Rules = []json.RawMessage{}
	}
	// make+copy preserves non-nil status even for empty slices, so JSON
	// marshals `[]` rather than `null`.
	out.RulesWarnings = make([]string, len(d.RulesWarnings))
	copy(out.RulesWarnings, d.RulesWarnings)
	return out
}

func cloneServers(servers []vless.Server) []vless.Server {
	if servers == nil {
		return []vless.Server{}
	}
	out := make([]vless.Server, len(servers))
	for i, srv := range servers {
		out[i] = srv
		if srv.Params != nil {
			p := make(map[string]string, len(srv.Params))
			for k, v := range srv.Params {
				p[k] = v
			}
			out[i].Params = p
		}
	}
	return out
}

// newSubscriptionID returns a globally-unique ID for a new subscription.
// Format: "sub-<unix>-<6 hex>". Time prefix sorts naturally; hex suffix
// disambiguates within the same second.
func newSubscriptionID() string {
	var buf [3]byte
	_, _ = rand.Read(buf[:])
	return fmt.Sprintf("sub-%d-%s", time.Now().Unix(), hex.EncodeToString(buf[:]))
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
