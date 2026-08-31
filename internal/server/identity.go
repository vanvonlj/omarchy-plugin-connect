package server

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/auth"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/device"
)

// tokenCookie carries a paired device's bearer token.
const tokenCookie = "omarchy_connect_token"

type ctxKey int

const deviceKey ctxKey = 0

// deviceFrom returns the device that made a request. It is never nil inside a
// handler: the middleware refuses anything it cannot identify.
func deviceFrom(ctx context.Context) *device.Device {
	d, _ := ctx.Value(deviceKey).(*device.Device)
	return d
}

// identify resolves a caller to a device, or refuses.
//
// Two routes in, and both end at the same place: a Device with a capability.
// That is what lets the plugin show one list, and what lets attach ask a single
// question -- may this device type? -- without caring how it got here.
func (s *Server) identify(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d, err := s.resolve(r)
		if err != nil {
			s.log.Warn("refused", "remote", r.RemoteAddr, "path", r.URL.Path, "reason", err)
			// A bare 403. The reason goes to the log and the desktop, never to
			// whoever was turned away.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// Best-effort: a device that cannot be touched is still a device that
		// passed identification, and failing the request would be worse.
		if err := s.devices.Touch(d.ID); err != nil {
			s.log.Debug("touch failed", "device", d.ID, "err", err)
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), deviceKey, d)))
	})
}

func (s *Server) resolve(r *http.Request) (*device.Device, error) {
	// Tailscale first. It is the stronger claim: an identity the network layer
	// asserts, rather than a secret the client presents and could have had
	// taken from it.
	who, err := s.tn.WhoIs(r.Context(), r.RemoteAddr)
	if err == nil {
		decision := auth.AdmitTailnet(auth.Self{UserID: s.tn.UserID}, who)
		if !decision.Allowed {
			return nil, errors.New(decision.Reason)
		}
		d, err := s.devices.RegisterTailnet(string(who.Node.StableID), tailnetName(decision.Peer))
		if err != nil {
			return nil, err
		}
		if d.Blocked {
			return nil, errors.New("device is blocked: " + d.Name)
		}
		return d, nil
	}

	// Otherwise a paired device's token. This is the path a phone with no
	// Tailscale takes once the LAN listener exists.
	if token := bearerToken(r); token != "" {
		d, err := s.devices.FindByToken(token)
		if err == nil {
			if d.Blocked {
				return nil, errors.New("device is blocked: " + d.Name)
			}
			return d, nil
		}
		return nil, errors.New("unrecognised device token")
	}

	return nil, errors.New("no tailnet identity and no device token")
}

// bearerToken reads the token from a cookie, falling back to the Authorization
// header so a script or another daemon can use the same API as the browser.
func bearerToken(r *http.Request) string {
	if c, err := r.Cookie(tokenCookie); err == nil && c.Value != "" {
		return c.Value
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
	}
	return ""
}

// tailnetName turns a MagicDNS name into something readable in a device list:
// "omarchy-hp.tail18c58.ts.net." becomes "omarchy-hp".
func tailnetName(peer string) string {
	name := strings.TrimSuffix(peer, ".")
	if i := strings.Index(name, "."); i > 0 {
		name = name[:i]
	}
	if name == "" {
		return "unknown device"
	}
	return name
}

// requireWrite refuses a request from a read-only device.
func (s *Server) requireWrite(w http.ResponseWriter, r *http.Request) bool {
	d := deviceFrom(r.Context())
	if d == nil || !d.EffectiveCapability().CanWrite() {
		http.Error(w, "this device is read-only", http.StatusForbidden)
		return false
	}
	return true
}
