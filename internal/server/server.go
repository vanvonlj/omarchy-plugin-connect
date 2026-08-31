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

	"github.com/vanvonlj/omarchy-plugin-connect/internal/auth"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/config"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/transport"
)

// Server serves the daemon over the tailnet.
type Server struct {
	tn      *transport.Tailnet
	cfg     config.Config
	log     *slog.Logger
	version string
}

// New returns a server bound to a probed tailnet node.
func New(tn *transport.Tailnet, cfg config.Config, log *slog.Logger, version string) *Server {
	return &Server{tn: tn, cfg: cfg, log: log, version: version}
}

// Handler returns the tailnet request handler, admission included.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return s.admitTailnet(mux)
}

// admitTailnet refuses any request whose peer Tailscale will not vouch for.
//
// The check is on every request rather than once per connection: a decision
// cached for the life of a keep-alive connection would outlive a revocation,
// and revocation that does not take effect immediately is not revocation.
func (s *Server) admitTailnet(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		who, err := s.tn.WhoIs(r.Context(), r.RemoteAddr)
		if err != nil {
			// A lookup failure is a refusal. Tailscale not being able to
			// identify a caller is exactly the case this gate exists for, so it
			// must not be treated as "unknown, therefore proceed".
			s.log.Warn("admission lookup failed", "remote", r.RemoteAddr, "err", err)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		decision := auth.AdmitTailnet(auth.Self{UserID: s.tn.UserID}, who)
		if !decision.Allowed {
			s.log.Warn("refused", "peer", decision.Peer, "reason", decision.Reason)
			// The caller gets a bare 403. The reason goes to the log and the
			// desktop, not to whoever was turned away.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
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
