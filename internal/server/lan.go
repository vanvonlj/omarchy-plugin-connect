package server

import (
	"net/http"
	"strings"
)

// identifyLAN resolves a caller on the tier-2 listener.
//
// It deliberately never consults WhoIs, and that is a security boundary rather
// than an optimisation. The tailnet path trusts Tailscale's assertion about a
// source address; on a LAN socket, the source address is whatever arrived on
// the wire. A packet reaching this listener with a spoofed 100.x source would
// otherwise resolve to a real tailnet peer and be admitted with no token at
// all. On this listener a device token is the only evidence accepted.
func (s *Server) identifyLAN(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Two things must be reachable before a device exists, or pairing could
		// never happen over the LAN: the page itself, and the endpoint that
		// redeems a code. The code is the credential in both cases -- 128 bits
		// of it -- and everything that touches a session stays behind a device.
		if isPairingRoute(r) {
			next.ServeHTTP(w, r)
			return
		}

		token := bearerToken(r)
		if token == "" {
			s.log.Warn("lan: refused, no device token", "remote", r.RemoteAddr, "path", r.URL.Path)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		d, err := s.devices.FindByToken(token)
		if err != nil {
			s.log.Warn("lan: refused, unrecognised token", "remote", r.RemoteAddr)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if d.Blocked {
			s.log.Warn("lan: refused, device blocked", "device", d.Name)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		if err := s.devices.Touch(d.ID); err != nil {
			s.log.Debug("touch failed", "device", d.ID, "err", err)
		}
		next.ServeHTTP(w, withDevice(r, d))
	})
}

// isPairingRoute reports whether a request may proceed without a device.
//
// The allowance is narrow on purpose: the static app shell, which is inert
// markup and script, and the single endpoint that trades a pairing code for a
// token. Anything that lists or attaches to a session is not on this list.
func isPairingRoute(r *http.Request) bool {
	if r.Method == http.MethodPost && r.URL.Path == "/api/pair" {
		return true
	}
	if r.Method != http.MethodGet {
		return false
	}
	p := r.URL.Path
	if p == "/" {
		return true
	}
	// Static assets only, matched by extension rather than by prefix so a new
	// API route can never fall through into the unauthenticated set by
	// accident.
	for _, ext := range []string{".html", ".js", ".css", ".png", ".svg", ".ico", ".webmanifest"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}
