package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/voledyaev/yonder/internal/services"
	"github.com/voledyaev/yonder/internal/state"
	"github.com/voledyaev/yonder/internal/vless"
	"github.com/voledyaev/yonder/internal/xray"
)

const (
	userAgent       = "yonder/0.2"
	maxLabelLen     = 100
	maxSourceLen    = 4096
)

// Handler is the request handler set. One instance is shared across all
// requests and is safe for concurrent use because each method either
// reads atomic state or mutates it via the locked state.State methods.
type Handler struct {
	state          *state.State
	xrayConfigsDir string
	logger         *log.Logger
	httpClient     *http.Client
	staticFS       fs.FS

	// applyCh signals the apply worker that state has changed and xkeen
	// needs to be regenerated + restarted. Buffered to 1 so concurrent
	// requests coalesce: at most one pending apply is queued at a time.
	// The worker always reads the latest state inside the loop, so a
	// coalesced apply still reflects the final intent. See applyLoop.
	applyCh chan struct{}
}

func (h *Handler) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/state", h.getState)
	mux.HandleFunc("GET /api/health", h.getHealth)

	// Subscription CRUD. Each subscription appears as its own card in the
	// UI; servers from all subscriptions feed into the same active-server
	// pool, addressed by composite (subscription_id, server_id).
	mux.HandleFunc("POST /api/subscriptions", h.postAddSubscription)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", h.deleteSubscription)
	mux.HandleFunc("POST /api/subscriptions/{id}/refresh", h.refreshSubscription)
	mux.HandleFunc("PATCH /api/subscriptions/{id}", h.patchSubscription)

	mux.HandleFunc("POST /api/server", h.postServer)
	mux.HandleFunc("POST /api/toggle", h.postToggle)
	mux.HandleFunc("POST /api/rules-url", h.postRulesURL)
	mux.HandleFunc("POST /api/rules/refresh", h.postRulesRefresh)
	mux.HandleFunc("/api/", h.unknownAPI) // catch-all for /api/* misses
	mux.Handle("/", h.staticHandler())
}

// --- response helpers ----------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, `{"error":"internal marshal"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON decodes the request body into v. Returns nil on empty body.
func readJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	if r.ContentLength > subscriptionMaxBody {
		return fmt.Errorf("body too large (>%d bytes)", subscriptionMaxBody)
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, subscriptionMaxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// --- static --------------------------------------------------------------

func (h *Handler) staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(h.staticFS))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

// --- API: meta -----------------------------------------------------------

func (h *Handler) getState(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, h.state.Snapshot())
}

func (h *Handler) getHealth(w http.ResponseWriter, _ *http.Request) {
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "host": host})
}

func (h *Handler) unknownAPI(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "unknown endpoint")
}

// --- API: subscriptions --------------------------------------------------

type addSubscriptionReq struct {
	Label  string `json:"label"`
	Source string `json:"source"`
}

func (h *Handler) postAddSubscription(w http.ResponseWriter, r *http.Request) {
	var req addSubscriptionReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	label := strings.TrimSpace(req.Label)
	source := strings.TrimSpace(req.Source)
	if err := validateLabelLength(label); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateSource(source); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if label == "" {
		label = deriveLabel(source)
	}

	servers, err := h.fetchAndParseSubscription(source)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errFetchFailed) {
			code = http.StatusBadGateway
		}
		writeError(w, code, err.Error())
		return
	}
	if len(servers) == 0 {
		writeError(w, http.StatusBadRequest, "no usable servers in subscription")
		return
	}

	if _, err := h.state.AddSubscription(label, source, servers); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	// New subscription doesn't change the active-server selection, but the
	// next /api/state poll will surface the new card. No apply needed —
	// xkeen config only changes when active server or rules change.
	writeJSON(w, http.StatusOK, h.state.Snapshot())
}

func (h *Handler) deleteSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.state.HasSubscription(id) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown subscription: %q", id))
		return
	}
	prev := h.state.Snapshot()
	affected := prev.ActiveServer != nil && prev.ActiveServer.SubscriptionID == id

	if _, err := h.state.DeleteSubscription(id); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, h.state.Snapshot())
	// Only trigger apply when the deletion actually changes runtime state
	// (i.e. the active server vanished). Pure list edits don't need xkeen.
	if affected {
		h.requestApply()
	}
}

func (h *Handler) refreshSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.state.HasSubscription(id) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown subscription: %q", id))
		return
	}
	// Find the source for this subscription. Re-fetched on each refresh
	// (URL) or re-parsed in place (inline vless://).
	var source string
	for _, sub := range h.state.Snapshot().Subscriptions {
		if sub.ID == id {
			source = sub.Source
			break
		}
	}

	servers, err := h.fetchAndParseSubscription(source)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errFetchFailed) {
			code = http.StatusBadGateway
		}
		writeError(w, code, err.Error())
		return
	}
	if len(servers) == 0 {
		writeError(w, http.StatusBadRequest, "no usable servers in subscription")
		return
	}

	prev := h.state.Snapshot()
	wasActiveHere := prev.ActiveServer != nil && prev.ActiveServer.SubscriptionID == id

	if _, err := h.state.ReplaceSubscriptionServers(id, servers); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, h.state.Snapshot())
	// If the active server was inside this subscription, the refresh may
	// have changed (or removed) it; trigger an apply to reconcile xkeen.
	if wasActiveHere {
		h.requestApply()
	}
}

type patchSubscriptionReq struct {
	Label string `json:"label"`
}

func (h *Handler) patchSubscription(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.state.HasSubscription(id) {
		writeError(w, http.StatusNotFound, fmt.Sprintf("unknown subscription: %q", id))
		return
	}
	var req patchSubscriptionReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	label := strings.TrimSpace(req.Label)
	if err := validateLabelLength(label); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Empty label = "reset to auto-derived default" — look up the
	// subscription's source and rebuild.
	if label == "" {
		for _, sub := range h.state.Snapshot().Subscriptions {
			if sub.ID == id {
				label = deriveLabel(sub.Source)
				break
			}
		}
	}
	if _, err := h.state.RenameSubscription(id, label); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, h.state.Snapshot())
}

// --- API: server selection -----------------------------------------------

type serverReq struct {
	// Both nil → deselect (set active to nil).
	SubscriptionID *string `json:"subscription_id"`
	ServerID       *string `json:"server_id"`
}

func (h *Handler) postServer(w http.ResponseWriter, r *http.Request) {
	var req serverReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var newRef *state.ActiveServerRef
	if req.SubscriptionID != nil && req.ServerID != nil &&
		*req.SubscriptionID != "" && *req.ServerID != "" {
		if !h.state.HasServer(*req.SubscriptionID, *req.ServerID) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"unknown (subscription_id, server_id): (%q, %q)",
				*req.SubscriptionID, *req.ServerID))
			return
		}
		newRef = &state.ActiveServerRef{
			SubscriptionID: *req.SubscriptionID,
			ServerID:       *req.ServerID,
		}
	}
	if _, err := h.state.Update(func(d *state.Data) { d.ActiveServer = newRef }); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	h.respondAfterApply(w)
}

// --- API: toggle ---------------------------------------------------------

type toggleReq struct {
	On bool `json:"on"`
}

func (h *Handler) postToggle(w http.ResponseWriter, r *http.Request) {
	var req toggleReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.On && h.state.ActiveServer() == nil {
		writeError(w, http.StatusBadRequest, "no active server selected")
		return
	}
	if _, err := h.state.Update(func(d *state.Data) { d.VPNOn = req.On }); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}
	h.respondAfterApply(w)
}

// --- API: rules URL ------------------------------------------------------

type rulesURLReq struct {
	URL *string `json:"url"` // null clears, string sets
}

func (h *Handler) postRulesURL(w http.ResponseWriter, r *http.Request) {
	var req rulesURLReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Clear case: rules_url=null or "" → fall back to bundled default.
	if req.URL == nil || *req.URL == "" {
		if _, err := h.state.Update(func(d *state.Data) {
			d.RulesURL = ""
			d.RulesFetchedAt = ""
			d.Rules = []json.RawMessage{}
			d.RulesWarnings = []string{}
			d.RulesSkippedCount = 0
		}); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		h.respondAfterApply(w)
		return
	}

	url := *req.URL
	rules, err := h.fetchAndValidateRules(url)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errFetchFailed) {
			code = http.StatusBadGateway
		}
		writeError(w, code, err.Error())
		return
	}
	if _, err := h.state.Update(func(d *state.Data) {
		d.RulesURL = url
		d.RulesFetchedAt = nowISO()
		d.Rules = rules
		d.RulesWarnings = []string{}
		d.RulesSkippedCount = 0
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.respondAfterApply(w)
}

func (h *Handler) postRulesRefresh(w http.ResponseWriter, _ *http.Request) {
	snap := h.state.Snapshot()
	if snap.RulesURL == "" {
		writeError(w, http.StatusBadRequest, "no rules_url configured")
		return
	}
	rules, err := h.fetchAndValidateRules(snap.RulesURL)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, errFetchFailed) {
			code = http.StatusBadGateway
		}
		writeError(w, code, err.Error())
		return
	}
	if _, err := h.state.Update(func(d *state.Data) {
		d.RulesFetchedAt = nowISO()
		d.Rules = rules
		d.RulesWarnings = []string{}
		d.RulesSkippedCount = 0
	}); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.respondAfterApply(w)
}

// --- shared apply path ---------------------------------------------------

// respondAfterApply writes the post-mutation state snapshot immediately,
// then schedules the proxy regenerate+restart to run asynchronously.
//
// Why async: `xkeen -restart` takes ~5s normally and during that window
// the LAN-side iptables tproxy rules get re-installed. Returning the
// response before kicking off xkeen lets the browser get its ack
// regardless of how long the apply takes. Failures surface via
// state.LastError / LastApply, picked up by the frontend's 10s poll.
//
// The state.Applying flag is set true here so the response (and any
// subsequent /api/state poll) tells the UI to disable controls until the
// worker clears the flag at the end of its iteration.
func (h *Handler) respondAfterApply(w http.ResponseWriter) {
	snap, _ := h.state.Update(func(d *state.Data) { d.Applying = true })
	writeJSON(w, http.StatusOK, snap)
	h.requestApply()
}

// requestApply nudges the apply worker. Non-blocking — if an apply is
// already queued, the request coalesces (the worker re-reads state when
// it runs, so the final intent is what gets applied).
func (h *Handler) requestApply() {
	select {
	case h.applyCh <- struct{}{}:
		h.logger.Println("requestApply: signal queued")
	default:
		h.logger.Println("requestApply: signal dropped (channel full or worker stuck)")
	}
}

// applyLoop is the single worker that drives xkeen restarts. Runs for the
// lifetime of the process; caller passes ctx for clean shutdown.
func (h *Handler) applyLoop(ctx context.Context) {
	h.logger.Println("applyLoop: started")
	defer h.logger.Println("applyLoop: exited")
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.applyCh:
			ok, msg := h.regenerateAndRestart()
			// Always clear Applying at end of iteration. If another signal
			// is already queued, the next handler call will flip it back
			// to true before its response is written; UI sees only a brief
			// false moment between iterations.
			if _, err := h.state.Update(func(d *state.Data) {
				d.Applying = false
				d.LastApply = &state.ApplyResult{At: nowISO(), OK: ok, Msg: msg}
				if ok {
					d.LastError = ""
				} else {
					d.LastError = msg
				}
			}); err != nil {
				h.logger.Printf("apply: save last_error failed: %v", err)
			}
			h.logger.Printf("apply: ok=%v msg=%q", ok, msg)
		}
	}
}

// regenerateAndRestart rewrites the two xray configs we own from current
// state, then restarts (or stops) xkeen depending on vpn_on. Runs only
// inside the apply worker — never called directly from request handlers.
func (h *Handler) regenerateAndRestart() (bool, string) {
	snap := h.state.Snapshot()
	server := h.state.ActiveServer()
	var rules []json.RawMessage
	if len(snap.Rules) > 0 {
		rules = snap.Rules
	}
	if err := xray.WriteXKeenSplit(server, rules, h.xrayConfigsDir); err != nil {
		return false, fmt.Sprintf("write config failed: %v", err)
	}
	if snap.VPNOn {
		return services.Restart()
	}
	return services.Stop()
}

// --- fetching ------------------------------------------------------------

var errFetchFailed = errors.New("fetch failed")

// fetchAndParseSubscription resolves a subscription source to a list of
// VLESS servers. The source may be:
//
//   - an HTTP(S) URL — fetched, then parsed
//   - a literal `vless://...` URI (or newline-separated list of them) —
//     parsed in place without any network call
//
// validateSource already enforces one of these two shapes; this function
// just dispatches on the prefix.
func (h *Handler) fetchAndParseSubscription(source string) ([]vless.Server, error) {
	var raw []byte
	if strings.HasPrefix(source, "vless://") {
		raw = []byte(source)
	} else {
		fetched, err := h.fetchURL(source)
		if err != nil {
			return nil, err
		}
		raw = fetched
	}
	servers, err := vless.ParseSubscription(raw)
	if err != nil {
		return nil, fmt.Errorf("subscription parse failed: %v", err)
	}
	return servers, nil
}

func (h *Handler) fetchURL(url string) ([]byte, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFetchFailed, err)
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFetchFailed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%w: HTTP %s", errFetchFailed, resp.Status)
	}
	// Read at most max+1 to detect overflow with one read.
	buf, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionMaxBody+1))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errFetchFailed, err)
	}
	if len(buf) > subscriptionMaxBody {
		return nil, fmt.Errorf("response too large (>%d KB limit)", subscriptionMaxBody/1024)
	}
	return buf, nil
}

func (h *Handler) fetchAndValidateRules(url string) ([]json.RawMessage, error) {
	raw, err := h.fetchURL(url)
	if err != nil {
		return nil, err
	}
	return parseXrayRules(raw)
}

// --- validators ----------------------------------------------------------

// validateLabelLength checks the size cap only — empty labels are accepted
// at both add-subscription and rename time, and resolved into a derived
// default via deriveLabel by the caller.
func validateLabelLength(s string) error {
	if len(s) > maxLabelLen {
		return fmt.Errorf("label is too long (max %d chars)", maxLabelLen)
	}
	return nil
}

func validateSource(s string) error {
	if s == "" {
		return errors.New("source is required")
	}
	if len(s) > maxSourceLen {
		return fmt.Errorf("source is too long (max %d chars)", maxSourceLen)
	}
	switch {
	case strings.HasPrefix(s, "http://"),
		strings.HasPrefix(s, "https://"),
		strings.HasPrefix(s, "vless://"):
		return nil
	default:
		return errors.New("source must start with http://, https://, or vless://")
	}
}

// deriveLabel builds a sensible default label from a subscription source.
// For URLs we use the hostname; for inline vless:// links the embedded host
// (which is the proxy endpoint, not the link's UUID — safe to display).
// Always returns a non-empty string.
func deriveLabel(source string) string {
	s := strings.TrimSpace(source)
	if strings.HasPrefix(s, "vless://") {
		if srv, err := vless.ParseLink(s); err == nil && srv.Host != "" {
			return srv.Host
		}
		return "vless link"
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" {
		return u.Host
	}
	return "Subscription"
}

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
