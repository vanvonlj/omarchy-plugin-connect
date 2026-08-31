// Package server wires listeners to handlers.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/config"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/device"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/pairing"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/transport"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/webui"
)

// Server serves the daemon over the tailnet.
type Server struct {
	tn      *transport.Tailnet
	cfg     config.Config
	log     *slog.Logger
	version string
	devices *device.Store
	pairing *pairing.Store
}

// New returns a server bound to a probed tailnet node.
func New(tn *transport.Tailnet, cfg config.Config, log *slog.Logger, version string, devices *device.Store, pairs *pairing.Store) *Server {
	return &Server{tn: tn, cfg: cfg, log: log, version: version, devices: devices, pairing: pairs}
}

// Handler returns the tailnet request handler, admission included.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{name}/attach", s.handleAttach)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/pair", s.handlePair)
	mux.Handle("GET /", webui.Handler())
	return s.identify(mux)
}

type health struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	Node    string `json:"node"`
	TLS     bool   `json:"tls"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(health{
		Status:  "ok",
		Version: s.version,
		Node:    s.tn.DNSName,
		TLS:     s.tn.CertsAvailable,
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	lns, err := s.tn.Listen(ctx, s.cfg.Port)
	if err != nil {
		return err
	}

	tlsCfg := s.tn.TLSConfig()
	srv := &http.Server{
		Handler:           s.Handler(),
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}

	if tlsCfg == nil {
		// Loud, and repeated in `status` and the plugin. A tailnet without
		// certificates still carries the terminal, but the browser will not
		// grant an http:// origin a secure context, so the PWA install, the
		// service worker, and push are all gone. That is worth saying every
		// single start rather than leaving someone to wonder why the install
		// button never appears.
		s.log.Warn("serving without TLS: tailnet has no HTTPS certificates",
			"fix", "enable DNS > HTTPS Certificates in the Tailscale admin console",
			"lost", "PWA install, service worker, web push")
	}

	var wg sync.WaitGroup
	errs := make(chan error, len(lns))
	for _, ln := range lns {
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			var err error
			if tlsCfg != nil {
				err = srv.ServeTLS(ln, "", "")
			} else {
				err = srv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				errs <- err
			}
		}(ln)
	}

	s.log.Info("listening", "url", s.tn.URL(s.cfg.Port), "addrs", len(lns))

	select {
	case <-ctx.Done():
	case err := <-errs:
		// One listener failing takes the daemon down rather than leaving it
		// half-reachable, which is far harder to diagnose from a phone.
		shutdown(srv)
		wg.Wait()
		return err
	}

	shutdown(srv)
	wg.Wait()
	return nil
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
