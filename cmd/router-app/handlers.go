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
	"os"
	"strings"
	"time"

	"github.com/voledyaev/yonder/internal/services"
	"github.com/voledyaev/yonder/internal/state"
	"github.com/voledyaev/yonder/internal/vless"
	"github.com/voledyaev/yonder/internal/xray"
)

const userAgent = "yonder/0.2"

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
	mux.HandleFunc("POST /api/subscription", h.postSubscription)
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

// --- API: subscription ---------------------------------------------------

type subscriptionReq struct {
	URL string `json:"url"`
}

func (h *Handler) postSubscription(w http.ResponseWriter, r *http.Request) {
	var req subscriptionReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	url := strings.TrimSpace(req.URL)
	if url == "" {
		writeError(w, http.StatusBadRequest, "missing 'url'")
		return
	}

	raw, err := h.fetchURL(url)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("subscription fetch failed: %v", err))
		return
	}
	servers, err := vless.ParseSubscription(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("subscription parse failed: %v", err))
		return
	}
	if len(servers) == 0 {
		writeError(w, http.StatusBadRequest, "no usable servers in subscription")
		return
	}

	prevSnap := h.state.Snapshot()
	wasOn := prevSnap.VPNOn
	oldActive := prevSnap.ActiveServerID

	snap, err := h.state.SetServers(servers, url, nowISO())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("save state: %v", err))
		return
	}

	// If VPN was on and the previously-selected server is gone, fail safe:
	// turn VPN off so the apply worker stops the proxy on its next tick.
	if wasOn && oldActive != "" && snap.ActiveServerID == "" {
		snap, _ = h.state.Update(func(d *state.Data) { d.VPNOn = false })
		h.requestApply()
	}
	writeJSON(w, http.StatusOK, snap)
}

// --- API: server selection -----------------------------------------------

type serverReq struct {
	ID *string `json:"id"` // pointer so null is distinguishable from missing
}

func (h *Handler) postServer(w http.ResponseWriter, r *http.Request) {
	var req serverReq
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	snap := h.state.Snapshot()
	var newID string
	if req.ID != nil {
		newID = *req.ID
	}
	if newID != "" {
		known := false
		for _, s := range snap.Servers {
			if s.ID == newID {
				known = true
				break
			}
		}
		if !known {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown server id: %q", newID))
			return
		}
	}
	if _, err := h.state.Update(func(d *state.Data) { d.ActiveServerID = newID }); err != nil {
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
// Why async: `xkeen -restart` takes up to 90s and re-installs the LAN-side
// iptables tproxy rules during that window. The mid-flight TCP connection
// from the browser to /api/toggle can get torn down as a side effect,
// leaving the UI stuck on a request that never returns. Returning the
// response before kicking off xkeen avoids the entire window. Failures
// surface via state.LastError, picked up by the frontend's 10s poll.
func (h *Handler) respondAfterApply(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, h.state.Snapshot())
	h.requestApply()
}

// requestApply nudges the apply worker. Non-blocking — if an apply is
// already queued, the request coalesces (the worker re-reads state when
// it runs, so the final intent is what gets applied).
func (h *Handler) requestApply() {
	select {
	case h.applyCh <- struct{}{}:
	default:
	}
}

// applyLoop is the single worker that drives xkeen restarts. Runs for the
// lifetime of the process; caller passes ctx for clean shutdown.
func (h *Handler) applyLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.applyCh:
			ok, msg := h.regenerateAndRestart()
			if _, err := h.state.Update(func(d *state.Data) {
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

func nowISO() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
