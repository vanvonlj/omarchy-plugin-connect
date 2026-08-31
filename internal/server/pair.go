package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/device"
	"github.com/vanvonlj/omarchy-plugin-connect/internal/pairing"
)

type meResponse struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	Capability string `json:"capability"`
	CanWrite   bool   `json:"canWrite"`
}

// handleMe tells the client what it is allowed to do.
//
// The UI needs this to say "read-only" up front rather than letting someone
// type into a terminal for a while and wonder why nothing happens.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	d := deviceFrom(r.Context())
	cap := d.EffectiveCapability()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(meResponse{
		ID:         d.ID,
		Name:       d.Name,
		Kind:       string(d.Kind),
		Capability: string(cap),
		CanWrite:   cap.CanWrite(),
	})
}

type pairRequest struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type pairResponse struct {
	// Token is empty when pairing named an already-identified device rather
	// than creating a new one: there is no new credential to hand back.
	Token string `json:"token,omitempty"`
	ID    string `json:"id"`
	Name  string `json:"name"`
}

// handlePair redeems a pairing code and issues a device token.
//
// The token is what a phone without Tailscale will present on later visits. A
// tailnet peer does not need one -- it is identified on every request -- but is
// issued one anyway, so a device that leaves the tailnet keeps working and the
// token path is exercised rather than sitting untested until it matters.
func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	var req pairRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if err := s.pairing.Redeem(req.Code); err != nil {
		// Both "wrong code" and "no pairing in progress" answer the same way.
		// Distinguishing them tells an unknown caller whether a pairing is
		// open, which is the one bit worth not giving away.
		s.log.Warn("pairing refused", "remote", r.RemoteAddr, "reason", err)
		if errors.Is(err, pairing.ErrBadCode) || errors.Is(err, pairing.ErrNoPending) {
			http.Error(w, "invalid or expired pairing code", http.StatusForbidden)
			return
		}
		http.Error(w, "pairing failed", http.StatusInternalServerError)
		return
	}

	name := req.Name
	if name == "" {
		name = "Paired device"
	}

	// A caller that already has an identity does not need a second one.
	//
	// Scanning the QR from a phone that is already on the tailnet would
	// otherwise mint a *paired* device while every subsequent request kept
	// resolving to the *tailnet* device -- two rows in the panel for one phone,
	// and promoting the wrong one silently doing nothing. Pairing from a known
	// device names it instead, which is what someone scanning a QR labelled
	// "add this device" actually means.
	if existing := deviceFrom(r.Context()); existing != nil && existing.Kind == device.Tailnet {
		if err := s.devices.Rename(existing.ID, name); err != nil {
			s.log.Error("renaming device on pair", "err", err)
			http.Error(w, "pairing failed", http.StatusInternalServerError)
			return
		}
		s.log.Info("named an existing tailnet device", "device", existing.ID, "name", name)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pairResponse{ID: existing.ID, Name: name})
		return
	}

	d, token, err := s.devices.CreatePaired(name)
	if err != nil {
		s.log.Error("creating paired device", "err", err)
		http.Error(w, "pairing failed", http.StatusInternalServerError)
		return
	}

	s.log.Info("paired", "device", d.ID, "name", d.Name, "capability", d.Capability)

	// HttpOnly so page script cannot read the token; SameSite=Lax so it rides
	// an ordinary navigation from the QR scan. Secure is set because the tailnet
	// origin is HTTPS -- a cookie marked Secure is simply not sent over the
	// plain-HTTP LAN tier, which is the correct outcome rather than a bug.
	http.SetCookie(w, &http.Cookie{
		Name:     tokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.tn.CertsAvailable,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((365 * 24 * 60 * 60)),
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(pairResponse{Token: token, ID: d.ID, Name: d.Name})
}

// capabilityOf re-reads a device's capability from the store.
//
// Attach calls this on a timer rather than trusting the value it captured at
// connect time. Revocation that only takes effect on the next connection is not
// revocation: the whole point is to cut off a device that is currently holding
// a terminal open.
func (s *Server) capabilityOf(id string) (device.Capability, bool) {
	d, err := s.devices.Get(id)
	if err != nil {
		return device.Read, false
	}
	// Blocked reads the same as deleted here. Both mean "this device may not be
	// holding a terminal open", and the attach watcher acts on the one signal.
	if d.Blocked {
		return device.Read, false
	}
	return d.EffectiveCapability(), true
}
