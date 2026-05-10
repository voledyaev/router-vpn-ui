// Package state holds the persistent state of the yonder daemon.
//
// Single JSON file under $RVU_BASE_DIR. Atomic writes via write-temp-then-
// rename so a crash mid-write cannot corrupt saved state.
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/voledyaev/yonder/internal/vless"
)

const SchemaVersion = 1

// Data is the on-disk schema. It is also what Snapshot returns to callers.
type Data struct {
	Version               int               `json:"version"`
	SubscriptionURL       string            `json:"subscription_url"`
	SubscriptionFetchedAt string            `json:"subscription_fetched_at"`
	Servers               []vless.Server    `json:"servers"`
	ActiveServerID        string            `json:"active_server_id"`
	VPNOn                 bool              `json:"vpn_on"`
	RulesURL              string            `json:"rules_url"`
	RulesFetchedAt        string            `json:"rules_fetched_at"`
	Rules                 []json.RawMessage `json:"rules"` // parsed Xray routing rules
	RulesWarnings         []string          `json:"rules_warnings"`
	RulesSkippedCount     int               `json:"rules_skipped_count"`
	LastError             string            `json:"last_error"`
}

// Defaults returns a fresh Data with safe zero values. Slices are always
// non-nil so JSON marshals them as [] rather than null.
func Defaults() Data {
	return Data{
		Version:       SchemaVersion,
		Servers:       []vless.Server{},
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

// New opens the state file at path. If the file is missing or corrupt,
// defaults are loaded and no error is returned — the next Update will
// create or overwrite the file.
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
	// Ensure slice fields are non-nil even when the file omits them.
	if loaded.Servers == nil {
		loaded.Servers = []vless.Server{}
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

// ActiveServer returns a pointer to the currently selected server (or nil
// if none is selected or the selection points to a server that no longer
// exists in the list).
func (s *State) ActiveServer() *vless.Server {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data.ActiveServerID == "" {
		return nil
	}
	for i := range s.data.Servers {
		if s.data.Servers[i].ID == s.data.ActiveServerID {
			srv := s.data.Servers[i]
			// Copy params map so the caller can't mutate stored state.
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

// SetServers replaces the server list and clears active_server_id if the
// previously-selected server is no longer in the new list. Optionally
// updates subscription metadata in the same atomic write.
func (s *State) SetServers(servers []vless.Server, subURL, fetchedAt string) (Data, error) {
	return s.Update(func(d *Data) {
		ids := make(map[string]struct{}, len(servers))
		for _, srv := range servers {
			ids[srv.ID] = struct{}{}
		}
		if d.ActiveServerID != "" {
			if _, ok := ids[d.ActiveServerID]; !ok {
				d.ActiveServerID = ""
			}
		}
		d.Servers = servers
		if d.Servers == nil {
			d.Servers = []vless.Server{}
		}
		if subURL != "" {
			d.SubscriptionURL = subURL
		}
		if fetchedAt != "" {
			d.SubscriptionFetchedAt = fetchedAt
		}
	})
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
	if d.Servers != nil {
		out.Servers = make([]vless.Server, len(d.Servers))
		for i, srv := range d.Servers {
			out.Servers[i] = srv
			if srv.Params != nil {
				p := make(map[string]string, len(srv.Params))
				for k, v := range srv.Params {
					p[k] = v
				}
				out.Servers[i].Params = p
			}
		}
	}
	if d.Rules != nil {
		out.Rules = make([]json.RawMessage, len(d.Rules))
		for i, r := range d.Rules {
			b := make([]byte, len(r))
			copy(b, r)
			out.Rules[i] = b
		}
	}
	// make+copy preserves non-nil status even for empty slices, so JSON
	// marshals `[]` rather than `null`.
	out.RulesWarnings = make([]string, len(d.RulesWarnings))
	copy(out.RulesWarnings, d.RulesWarnings)
	return out
}
