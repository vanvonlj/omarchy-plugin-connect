// Package device stores the machines allowed to reach this daemon.
//
// Both halves of the identity model land here. A tailnet peer is identified by
// Tailscale and registers itself on first contact; a phone with no Tailscale
// pairs with a code and carries a token. Either way it becomes a Device with a
// capability, so the plugin's device list has one kind of row and promotion
// works the same for a phone and for another Omarchy box.
package device

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Capability is what a device is allowed to do.
type Capability string

const (
	// Read can list sessions and watch a terminal. It cannot type.
	Read Capability = "read"

	// Write can type. Reaching it is always a deliberate act at the desktop.
	Write Capability = "write"
)

// Valid reports whether c is a capability this daemon understands.
//
// Anything unrecognised is not a capability, and callers treat it as read.
// A typo in the config file must not silently grant more than it names.
func (c Capability) Valid() bool { return c == Read || c == Write }

// CanWrite is the single place that decides whether input reaches a terminal.
func (c Capability) CanWrite() bool { return c == Write }

// Kind distinguishes how a device proves who it is.
type Kind string

const (
	// Tailnet devices are vouched for by Tailscale on every request. They hold
	// no token, because there is nothing a token would add to an identity the
	// network layer already asserts.
	Tailnet Kind = "tailnet"

	// Paired devices carry a bearer token, issued once from a pairing code.
	Paired Kind = "paired"
)

// Device is one machine allowed to reach the daemon.
type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Kind       Kind       `json:"kind"`
	Capability Capability `json:"capability"`

	// NodeID is the Tailscale stable node id, for Kind == Tailnet. Stable ids
	// survive a node's IP changing, which its address does not.
	NodeID string `json:"nodeId,omitempty"`

	// TokenHash is the SHA-256 of the bearer token, for Kind == Paired. The
	// token itself is shown once at pairing and never stored: a stolen
	// devices.json must not be a stolen set of credentials.
	TokenHash string `json:"tokenHash,omitempty"`

	// Blocked refuses a device that cannot simply be deleted.
	//
	// Deleting a paired device is enough: its token dies with the record and
	// nothing can mint another without a new QR. A tailnet device is different
	// -- Tailscale keeps vouching for it, so a deleted record is recreated on
	// its very next request, read-only and with a fresh id. That makes deletion
	// a rename, not a revocation. Blocked is what makes the word honest.
	Blocked bool `json:"blocked,omitempty"`

	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"lastSeen,omitempty"`
}

// EffectiveCapability is what the device may actually do, treating anything
// unrecognised as read.
func (d *Device) EffectiveCapability() Capability {
	if d.Blocked {
		return Read
	}
	if d.Capability.Valid() {
		return d.Capability
	}
	return Read
}

// Store is the persisted device list.
//
// Two processes write it -- the daemon updating LastSeen, and the CLI changing
// capabilities from the plugin panel -- so every mutation is a read-modify-write
// under an exclusive file lock. Holding the whole list in memory and writing it
// back without one loses whichever change landed first.
type Store struct {
	path string
}

// Open returns a store backed by devices.json in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	return &Store{path: filepath.Join(dir, "devices.json")}, nil
}

// List returns every device, newest first.
func (s *Store) List() ([]Device, error) {
	var devices []Device
	err := s.withLock(false, func(d []Device) ([]Device, error) {
		devices = d
		return nil, errReadOnly
	})
	if err != nil && !errors.Is(err, errReadOnly) {
		return nil, err
	}
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Created.After(devices[j].Created)
	})
	return devices, nil
}

// errReadOnly tells withLock that a callback made no changes worth writing.
var errReadOnly = errors.New("read-only")

// ErrNotFound is returned for an id that matches no device.
var ErrNotFound = errors.New("no such device")

// Get returns one device by id.
func (s *Store) Get(id string) (*Device, error) {
	devices, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].ID == id {
			return &devices[i], nil
		}
	}
	return nil, ErrNotFound
}

// FindByNode returns the device registered for a Tailscale stable node id.
func (s *Store) FindByNode(nodeID string) (*Device, error) {
	if nodeID == "" {
		return nil, ErrNotFound
	}
	devices, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].Kind == Tailnet && devices[i].NodeID == nodeID {
			return &devices[i], nil
		}
	}
	return nil, ErrNotFound
}

// FindByToken returns the device holding a bearer token.
//
// The comparison is constant-time over the hash. A token lookup that returns
// early on the first differing byte leaks, to anyone who can time it, how much
// of a guess was correct.
func (s *Store) FindByToken(token string) (*Device, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	want := hashToken(token)

	devices, err := s.List()
	if err != nil {
		return nil, err
	}
	for i := range devices {
		if devices[i].Kind != Paired {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(devices[i].TokenHash), []byte(want)) == 1 {
			return &devices[i], nil
		}
	}
	return nil, ErrNotFound
}

// RegisterTailnet returns the device for a tailnet node, creating it read-only
// on first contact.
//
// Self-registration is what makes Omarchy -> Omarchy a matter of opening a URL,
// and it is safe precisely because the new device lands at Read: appearing in
// the list is not the same as being trusted to type.
func (s *Store) RegisterTailnet(nodeID, name string) (*Device, error) {
	var out *Device

	err := s.withLock(true, func(devices []Device) ([]Device, error) {
		for i := range devices {
			if devices[i].Kind == Tailnet && devices[i].NodeID == nodeID {
				// A blocked device is returned unchanged, blocked. It must not
				// have its LastSeen refreshed into looking active, and it must
				// certainly not be re-admitted.
				if devices[i].Blocked {
					out = &devices[i]
					return nil, errReadOnly
				}
				devices[i].LastSeen = time.Now()
				out = &devices[i]
				return devices, nil
			}
		}

		id, err := newID()
		if err != nil {
			return nil, err
		}
		d := Device{
			ID:         id,
			Name:       name,
			Kind:       Tailnet,
			Capability: Read,
			NodeID:     nodeID,
			Created:    time.Now(),
			LastSeen:   time.Now(),
		}
		devices = append(devices, d)
		out = &devices[len(devices)-1]
		return devices, nil
	})
	if err != nil && !errors.Is(err, errReadOnly) {
		return nil, err
	}
	return out, nil
}

// CreatePaired registers a device from a completed pairing and returns its
// bearer token. The token is returned exactly once and never stored in clear.
func (s *Store) CreatePaired(name string) (*Device, string, error) {
	token, err := newToken()
	if err != nil {
		return nil, "", err
	}

	var out *Device
	err = s.withLock(true, func(devices []Device) ([]Device, error) {
		id, err := newID()
		if err != nil {
			return nil, err
		}
		d := Device{
			ID:         id,
			Name:       name,
			Kind:       Paired,
			Capability: Read,
			TokenHash:  hashToken(token),
			Created:    time.Now(),
			LastSeen:   time.Now(),
		}
		devices = append(devices, d)
		out = &devices[len(devices)-1]
		return devices, nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, token, nil
}

// SetCapability promotes or demotes a device.
func (s *Store) SetCapability(id string, c Capability) error {
	if !c.Valid() {
		return fmt.Errorf("unknown capability %q", c)
	}
	return s.mutate(id, func(d *Device) { d.Capability = c })
}

// Rename changes a device's display name.
func (s *Store) Rename(id, name string) error {
	if name == "" {
		return errors.New("name cannot be empty")
	}
	return s.mutate(id, func(d *Device) { d.Name = name })
}

// Touch records that a device was seen, at most once a minute.
//
// The throttle exists because every request would otherwise take the file lock
// and rewrite the list, turning a terminal's keystroke traffic into a stream of
// disk writes and lock contention.
func (s *Store) Touch(id string) error {
	return s.withLock(true, func(devices []Device) ([]Device, error) {
		for i := range devices {
			if devices[i].ID == id {
				if time.Since(devices[i].LastSeen) < time.Minute {
					return nil, errReadOnly
				}
				devices[i].LastSeen = time.Now()
				return devices, nil
			}
		}
		return nil, errReadOnly
	})
}

// Revoke stops a device using the daemon, whatever kind it is.
//
// A paired device is deleted: its token hash goes with the record, so the token
// it holds stops matching anything. A tailnet device is blocked instead,
// because Tailscale will keep vouching for it and a deleted record would be
// recreated on its next request -- leaving someone who just revoked a lost
// laptop with a device list that looks clean and a daemon that still answers it.
func (s *Store) Revoke(id string) error {
	found := false
	err := s.withLock(true, func(devices []Device) ([]Device, error) {
		out := devices[:0]
		for _, d := range devices {
			if d.ID != id {
				out = append(out, d)
				continue
			}
			found = true
			if d.Kind == Tailnet {
				d.Blocked = true
				d.Capability = Read
				out = append(out, d)
			}
		}
		return out, nil
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// Unblock lets a blocked tailnet device back in, read-only.
func (s *Store) Unblock(id string) error {
	return s.mutate(id, func(d *Device) {
		d.Blocked = false
		d.Capability = Read
	})
}

func (s *Store) mutate(id string, fn func(*Device)) error {
	found := false
	err := s.withLock(true, func(devices []Device) ([]Device, error) {
		for i := range devices {
			if devices[i].ID == id {
				fn(&devices[i])
				found = true
				return devices, nil
			}
		}
		return nil, errReadOnly
	})
	if err != nil && !errors.Is(err, errReadOnly) {
		return err
	}
	if !found {
		return ErrNotFound
	}
	return nil
}

// withLock runs fn against the current device list under a file lock, writing
// back whatever it returns. Returning errReadOnly skips the write.
func (s *Store) withLock(write bool, fn func([]Device) ([]Device, error)) error {
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening device lock: %w", err)
	}
	defer lock.Close()

	how := syscall.LOCK_SH
	if write {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(lock.Fd()), how); err != nil {
		return fmt.Errorf("locking device store: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	devices, err := s.read()
	if err != nil {
		return err
	}

	next, err := fn(devices)
	if err != nil {
		return err
	}
	if !write {
		return nil
	}
	return s.write(next)
}

func (s *Store) read() ([]Device, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", s.path, err)
	}
	if len(raw) == 0 {
		return nil, nil
	}

	var devices []Device
	if err := json.Unmarshal(raw, &devices); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	return devices, nil
}

func (s *Store) write(devices []Device) error {
	if devices == nil {
		devices = []Device{}
	}
	raw, err := json.MarshalIndent(devices, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding devices: %w", err)
	}
	raw = append(raw, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, "devices.*.json")
	if err != nil {
		return fmt.Errorf("creating temp device file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)

	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, s.path)
}

func newID() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating device id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating device token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
