// yonderd is the daemon installed on the router. It serves the web UI on
// port 8080 and supervises the xray process via xkeen.
//
// Configuration is read entirely from environment variables, set by the
// init script that the installer deploys:
//
//	RVU_BASE_DIR          absolute path to per-router data dir
//	                      (typically /opt/yonder/data, which sits on the
//	                      USB drive via the Entware mount). state.json
//	                      and any future per-router files live here.
//	RVU_LISTEN            listen address, default ":8080".
//	RVU_XRAY_CONFIGS_DIR  override XKeen's configs directory; default
//	                      /opt/etc/xray/configs. Useful for local dev.
package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/voledyaev/yonder/internal/services"
	"github.com/voledyaev/yonder/internal/state"
	"github.com/voledyaev/yonder/internal/watchdog"
)

//go:embed static
var staticFS embed.FS

const (
	defaultListen          = ":8080"
	defaultXrayConfigsDir  = "/opt/etc/xray/configs"
	subscriptionMaxBody    = 1 << 20 // 1 MiB — subscription bodies are small
	subscriptionTimeout    = 30 * time.Second
	watchdogInterval       = 30 * time.Second
	watchdogBackoffMax     = 5 * time.Minute
	httpReadHeaderTimeout  = 10 * time.Second
	httpRequestTimeout     = 2 * time.Minute // covers subscription fetches
	gracefulShutdownPeriod = 5 * time.Second
)

func main() {
	baseDir := os.Getenv("RVU_BASE_DIR")
	if baseDir == "" {
		log.Fatal("RVU_BASE_DIR is not set; refusing to guess where to put state.json")
	}
	listen := envOr("RVU_LISTEN", defaultListen)
	xrayConfigsDir := envOr("RVU_XRAY_CONFIGS_DIR", defaultXrayConfigsDir)

	statePath := filepath.Join(baseDir, "state.json")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		log.Fatalf("create base dir %s: %v", baseDir, err)
	}

	st, err := state.New(statePath)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	logger := log.New(os.Stderr, "yonderd ", log.LstdFlags|log.Lmsgprefix)
	logger.Printf("listening on http://%s/", listen)
	logger.Printf("  state: %s", statePath)
	logger.Printf("  xray configs: %s/{04_outbounds.json,05_routing.json}",
		xrayConfigsDir)

	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("embed static subtree: %v", err)
	}

	h := &Handler{
		state:          st,
		xrayConfigsDir: xrayConfigsDir,
		logger:         logger,
		httpClient: &http.Client{
			Timeout: subscriptionTimeout,
		},
		staticFS: staticSub,
		applyCh:  make(chan struct{}, 1),
	}

	mux := http.NewServeMux()
	h.register(mux)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	go h.applyLoop(ctx)
	watchdog.Start(ctx, watchdogAdapter{st}, watchdogInterval, watchdogBackoffMax, os.Stderr)

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: httpReadHeaderTimeout,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("listen: %v", err)
		}
	}()

	<-ctx.Done()
	logger.Println("shutdown signal received")

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), gracefulShutdownPeriod)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Printf("shutdown: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// watchdogAdapter bridges *state.State + the services package to the
// watchdog.Deps interface. Keeps watchdog itself decoupled from concrete
// types.
type watchdogAdapter struct{ s *state.State }

func (w watchdogAdapter) VPNOn() bool                { return w.s.Snapshot().VPNOn }
func (w watchdogAdapter) IsRunning() bool            { return services.IsRunning() }
func (w watchdogAdapter) Restart() (bool, string)    { return services.Restart() }

