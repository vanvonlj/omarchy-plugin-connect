// Package pairing issues the short-lived codes that turn a phone into a device.
package pairing

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// TTL is how long a pairing code lives.
//
// Long enough to walk to the phone and scan; short enough that a QR left on
// screen after someone wanders off is not a standing invitation.
const TTL = 3 * time.Minute

// Pending is the one outstanding pairing, if any.
//
// One at a time, deliberately. A queue of live codes is a queue of ways in, and
// the panel only ever shows one QR.
type Pending struct {
	Code    string    `json:"code"`
	Created time.Time `json:"created"`
	Expires time.Time `json:"expires"`

	// Attempts counts failed redemptions.
	Attempts int `json:"attempts"`
}

// maxAttempts is how many wrong codes a pending pairing survives.
//
// Clearing on the first failure looks stricter and is worse. Once tier 2 exists
// the pairing endpoint is reachable by anything on the wifi, so a single wrong
// guess from a stranger would cancel the pairing you are in the middle of --
// a denial of service handed out for free. The code carries 128 bits of
// entropy, so guessing is not the threat that needs defending against; a small
// allowance keeps a typo or a double-scan from costing a re-pair.
const maxAttempts = 5

// Expired reports whether p can no longer be redeemed.
func (p *Pending) Expired() bool { return time.Now().After(p.Expires) }

// Store persists the pending pairing.
type Store struct {
	path string
}

// Open returns a store backed by pairing.json in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	return &Store{path: filepath.Join(dir, "pairing.json")}, nil
}

// ErrNoPending means there is no code to redeem.
var ErrNoPending = errors.New("no pairing in progress")

// ErrBadCode means the code did not match or has expired. The two are one
// error on purpose: telling a caller which of the two happened tells them
// whether they had the right code, which is most of what they wanted to learn.
var ErrBadCode = errors.New("invalid or expired pairing code")

// Create starts a new pairing, replacing any pending one.
func (s *Store) Create() (*Pending, error) {
	// 16 random bytes, not a six-digit code. The code travels by QR, so nobody
	// types it, and there is no usability argument left to trade against
	// brute-force resistance on a LAN listener.
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generating pairing code: %w", err)
	}

	now := time.Now()
	p := &Pending{
		Code:    base64.RawURLEncoding.EncodeToString(b),
		Created: now,
		Expires: now.Add(TTL),
	}
	if err := s.save(p); err != nil {
		return nil, err
	}
	return p, nil
}

// Peek returns the pending pairing without consuming it, for `status`.
func (s *Store) Peek() (*Pending, error) {
	p, err := s.load()
	if err != nil {
		return nil, err
	}
	if p == nil || p.Expired() {
		return nil, ErrNoPending
	}
	return p, nil
}

// Redeem consumes the pending pairing if code matches.
//
// A correct code clears it immediately. A wrong one counts against maxAttempts
// and clears it only once those run out, so a stranger on the wifi cannot
// cancel a pairing in progress with a single bad guess.
func (s *Store) Redeem(code string) error {
	p, err := s.load()
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNoPending
	}

	if p.Expired() {
		s.clear()
		return ErrBadCode
	}

	if subtle.ConstantTimeCompare([]byte(p.Code), []byte(code)) != 1 {
		p.Attempts++
		if p.Attempts >= maxAttempts {
			s.clear()
		} else {
			s.save(p)
		}
		return ErrBadCode
	}

	// Success consumes it. A code that survived a correct guess would pair
	// twice, which is the one outcome worse than making someone scan again.
	s.clear()
	return nil
}

// URL is what the QR encodes.
//
// The scheme is supplied by the caller rather than assumed: the tailnet origin
// is https and the LAN origin can never be, because the tailnet certificate
// carries one SAN and it is not a LAN address.
func (p *Pending) URL(scheme, host string, port int) string {
	u := url.URL{
		Scheme:   scheme,
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/",
		RawQuery: url.Values{"pair": {p.Code}}.Encode(),
	}
	return u.String()
}

func (s *Store) save(p *Pending) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return s.withLock(func() error {
		return os.WriteFile(s.path, append(raw, '\n'), 0o600)
	})
}

func (s *Store) clear() error {
	return s.withLock(func() error {
		err := os.Remove(s.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	})
}

func (s *Store) load() (*Pending, error) {
	var p *Pending
	err := s.withLock(func() error {
		raw, err := os.ReadFile(s.path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		p = &Pending{}
		return json.Unmarshal(raw, p)
	})
	return p, err
}

// withLock serialises access across the daemon and the CLI, which both touch
// this file: the CLI creates a code from the plugin panel, and the daemon
// redeems it when the phone arrives.
func (s *Store) withLock(fn func() error) error {
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening pairing lock: %w", err)
	}
	defer lock.Close()

	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking pairing store: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	return fn()
}
