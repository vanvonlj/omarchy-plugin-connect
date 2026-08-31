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

	// lan is nil when tier 2 is disabled or no address was found.
	lan *transport.LAN
}

// WithLAN enables the tier-2 listener.
func (s *Server) WithLAN(lan *transport.LAN) *Server {
	s.lan = lan
	return s
}

// New returns a server bound to a probed tailnet node.
func New(tn *transport.Tailnet, cfg config.Config, log *slog.Logger, version string, devices *device.Store, pairs *pairing.Store) *Server {
	return &Server{tn: tn, cfg: cfg, log: log, version: version, devices: devices, pairing: pairs}
}

// routes is the shared route table. The two listeners serve identical routes
// and differ only in how they identify a caller, which keeps tier 2 from
// quietly drifting into a different API.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleCreateSession)
	mux.HandleFunc("DELETE /api/sessions/{name}", s.handleKillSession)
	mux.HandleFunc("GET /api/sessions/{name}/attach", s.handleAttach)
	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/pair", s.handlePair)
	mux.Handle("GET /", webui.Handler())
	return mux
}

// Handler returns the tailnet request handler, admission included.
func (s *Server) Handler() http.Handler {
	return s.accessLog("tailnet", s.identify(s.routes()))
}

// LANHandler returns the tier-2 handler, which accepts device tokens only.
func (s *Server) LANHandler() http.Handler {
	return s.accessLog("lan", s.identifyLAN(s.routes()))
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

	// Every server that must be shut down before wg.Wait() returns.
	servers := []*http.Server{srv}

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

	// Tier 2, when enabled. A failure here is logged and does not stop the
	// daemon: losing the optional LAN listener must never take the tailnet
	// listener down with it.
	if s.lan != nil {
		lanSrv := &http.Server{
			Handler:           s.LANHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			ErrorLog:          slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
		}
		lanLn, err := s.lan.Listen(ctx, s.cfg.LAN.Port)
		if err != nil {
			s.log.Error("LAN listener failed to bind; tier 2 is off", "err", err)
		} else {
			// Registered for shutdown rather than deferred. A deferred shutdown
			// runs after wg.Wait(), and the wait includes this server's own
			// serve goroutine, which does not return until the server is shut
			// down -- so the daemon hangs on exit still holding the LAN port,
			// and the next start cannot bind it.
			servers = append(servers, lanSrv)
			s.log.Info("listening on the LAN",
				"url", s.lan.URL(s.cfg.LAN.Port), "iface", s.lan.Iface)
			if hint := s.lan.FirewallHint(s.cfg.LAN.Port); hint != "" {
				s.log.Warn("a firewall will drop LAN connections\n" + hint)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := lanSrv.Serve(lanLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
					s.log.Error("LAN listener stopped", "err", err)
				}
			}()
		}
	}

	select {
	case <-ctx.Done():
	case err := <-errs:
		// One listener failing takes the daemon down rather than leaving it
		// half-reachable, which is far harder to diagnose from a phone.
		shutdownAll(servers)
		wg.Wait()
		return err
	}

	shutdownAll(servers)
	wg.Wait()
	return nil
}

func shutdownAll(servers []*http.Server) {
	for _, srv := range servers {
		shutdown(srv)
	}
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}
