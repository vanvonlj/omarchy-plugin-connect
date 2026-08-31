package device

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

// A new device must never arrive able to type. This is the promise the README
// makes and the reason pairing is two steps.
func TestNewDevicesAreReadOnly(t *testing.T) {
	s := newStore(t)

	tailnet, err := s.RegisterTailnet("nodeABC", "omarchy-hp")
	if err != nil {
		t.Fatalf("RegisterTailnet: %v", err)
	}
	if tailnet.EffectiveCapability().CanWrite() {
		t.Error("a self-registered tailnet device arrived writable")
	}

	paired, _, err := s.CreatePaired("phone")
	if err != nil {
		t.Fatalf("CreatePaired: %v", err)
	}
	if paired.EffectiveCapability().CanWrite() {
		t.Error("a freshly paired device arrived writable")
	}
}

// An unrecognised capability must read as read, not as "not read, therefore
// write". A hand-edited or future-versioned devices.json must fail closed.
func TestUnknownCapabilityFailsClosed(t *testing.T) {
	for _, raw := range []string{"", "admin", "Write", "write ", "rw"} {
		d := &Device{Capability: Capability(raw)}
		if d.EffectiveCapability().CanWrite() {
			t.Errorf("capability %q was treated as writable", raw)
		}
	}
}

func TestSetCapabilityRejectsUnknown(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("node1", "box")

	if err := s.SetCapability(d.ID, Capability("superuser")); err == nil {
		t.Fatal("SetCapability accepted an unknown capability")
	}
}

func TestPromoteAndDemote(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("node1", "box")

	if err := s.SetCapability(d.ID, Write); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	got, _ := s.Get(d.ID)
	if !got.EffectiveCapability().CanWrite() {
		t.Fatal("device did not become writable")
	}

	if err := s.SetCapability(d.ID, Read); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}
	got, _ = s.Get(d.ID)
	if got.EffectiveCapability().CanWrite() {
		t.Fatal("device did not become read-only again")
	}
}

// Registering the same node twice must return the same device, not accumulate
// duplicates -- otherwise every reconnect from another Omarchy box would add a
// row to the panel and reset its capability to read.
func TestRegisterTailnetIsIdempotent(t *testing.T) {
	s := newStore(t)

	first, _ := s.RegisterTailnet("nodeABC", "omarchy-hp")
	if err := s.SetCapability(first.ID, Write); err != nil {
		t.Fatalf("SetCapability: %v", err)
	}

	second, err := s.RegisterTailnet("nodeABC", "omarchy-hp")
	if err != nil {
		t.Fatalf("RegisterTailnet: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("re-registration created a new device: %s != %s", second.ID, first.ID)
	}

	got, _ := s.Get(first.ID)
	if !got.EffectiveCapability().CanWrite() {
		t.Error("re-registration reset a promoted device back to read-only")
	}

	all, _ := s.List()
	if len(all) != 1 {
		t.Errorf("got %d devices, want 1", len(all))
	}
}

// The raw token must never be written to disk. A stolen devices.json should not
// be a stolen set of credentials.
func TestTokenIsNotStoredInClear(t *testing.T) {
	s := newStore(t)

	_, token, err := s.CreatePaired("phone")
	if err != nil {
		t.Fatalf("CreatePaired: %v", err)
	}
	if token == "" {
		t.Fatal("CreatePaired returned an empty token")
	}

	devices, _ := s.List()
	for _, d := range devices {
		if strings.Contains(d.TokenHash, token) || d.TokenHash == token {
			t.Fatal("the raw token was stored")
		}
	}

	// It must still be usable.
	found, err := s.FindByToken(token)
	if err != nil {
		t.Fatalf("FindByToken with the issued token: %v", err)
	}
	if found.Kind != Paired {
		t.Errorf("Kind = %q, want %q", found.Kind, Paired)
	}
}

func TestFindByTokenRejectsWrongToken(t *testing.T) {
	s := newStore(t)
	s.CreatePaired("phone")

	if _, err := s.FindByToken("not-the-token"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	// An empty token must not match a device with an empty hash by accident.
	if _, err := s.FindByToken(""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty token: err = %v, want ErrNotFound", err)
	}
}

// A tailnet device holds no token, so a token lookup must never return one.
// Without the Kind check, an empty presented token could match an empty hash.
func TestTailnetDeviceIsNotReachableByToken(t *testing.T) {
	s := newStore(t)
	s.RegisterTailnet("nodeABC", "omarchy-hp")

	if _, err := s.FindByToken(""); !errors.Is(err, ErrNotFound) {
		t.Fatal("a tailnet device was matched by an empty token")
	}
}

// Revoking a device that does not exist is an error; revoking a tailnet device
// twice is not, because the second call finds the block record it left behind.
// The per-kind outcomes are covered by TestRevokedTailnetDeviceStaysRefused and
// TestRevokedPairedDeviceIsDeleted.
func TestRevokeUnknownDevice(t *testing.T) {
	s := newStore(t)
	if err := s.Revoke("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestRevokeTailnetTwiceIsIdempotent(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("node1", "box")

	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	got, err := s.Get(d.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Blocked {
		t.Fatal("device is not blocked after two revocations")
	}
}

func TestRenameRejectsEmpty(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("node1", "box")

	if err := s.Rename(d.ID, ""); err == nil {
		t.Fatal("Rename accepted an empty name")
	}
}

func TestMutateUnknownDevice(t *testing.T) {
	s := newStore(t)
	if err := s.SetCapability("nope", Write); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// The daemon and the CLI both write this file. Concurrent registrations must
// all survive: a read-modify-write without the file lock loses whichever
// arrived first, and a device that silently vanishes from the panel is a bug
// nobody can reproduce on demand.
func TestConcurrentRegistrationsDoNotLoseDevices(t *testing.T) {
	s := newStore(t)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := s.RegisterTailnet(nodeID(i), "box"); err != nil {
				t.Errorf("RegisterTailnet(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	devices, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(devices) != n {
		t.Fatalf("got %d devices, want %d; concurrent writes were lost", len(devices), n)
	}
}

func nodeID(i int) string {
	return "node-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
}

// Revoking a tailnet device must actually stop it, not just rename it.
//
// Tailscale keeps vouching for the node, so a deleted record is recreated on
// the next request. Before Blocked existed, revoke did exactly that: the panel
// looked clean and the daemon still answered the device.
func TestRevokedTailnetDeviceStaysRefused(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("nodeABC", "omarchy-hp")
	s.SetCapability(d.ID, Write)

	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// The next request from the same node.
	again, err := s.RegisterTailnet("nodeABC", "omarchy-hp")
	if err != nil {
		t.Fatalf("RegisterTailnet after revoke: %v", err)
	}
	if !again.Blocked {
		t.Fatal("a revoked tailnet device came back unblocked")
	}
	if again.EffectiveCapability().CanWrite() {
		t.Fatal("a revoked tailnet device came back writable")
	}

	all, _ := s.List()
	if len(all) != 1 {
		t.Fatalf("got %d devices, want 1; the block record was not kept", len(all))
	}
}

// A paired device is deleted outright: its token hash goes with it, so the
// token it holds stops matching anything.
func TestRevokedPairedDeviceIsDeleted(t *testing.T) {
	s := newStore(t)
	d, token, _ := s.CreatePaired("phone")

	if err := s.Revoke(d.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.FindByToken(token); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked paired device's token still works")
	}
	all, _ := s.List()
	if len(all) != 0 {
		t.Fatalf("got %d devices, want 0", len(all))
	}
}

func TestUnblockRestoresReadOnly(t *testing.T) {
	s := newStore(t)
	d, _ := s.RegisterTailnet("nodeABC", "omarchy-hp")
	s.Revoke(d.ID)

	if err := s.Unblock(d.ID); err != nil {
		t.Fatalf("Unblock: %v", err)
	}
	got, _ := s.Get(d.ID)
	if got.Blocked {
		t.Error("device is still blocked")
	}
	if got.EffectiveCapability().CanWrite() {
		t.Error("unblock restored write; it must come back read-only")
	}
}

// A blocked device reports read even if its stored capability says write, so a
// caller that forgets to check Blocked still cannot type.
func TestBlockedDeviceReportsRead(t *testing.T) {
	d := &Device{Capability: Write, Blocked: true}
	if d.EffectiveCapability().CanWrite() {
		t.Fatal("a blocked device reported a writable capability")
	}
}
