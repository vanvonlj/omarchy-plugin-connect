package session

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// isolate points tmux at a throwaway socket directory for the duration of a
// test. TMUX_TMPDIR is inherited by the tmux processes the package execs, so a
// test can never see, resize, or kill a session the developer is actually
// using -- which matters here, because the package's whole job is attaching to
// other people's terminals.
func isolate(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}

	dir := t.TempDir()
	t.Setenv("TMUX_TMPDIR", dir)
	t.Cleanup(func() {
		exec.Command("tmux", "kill-server").Run()
	})
}

func newSession(t *testing.T, name string, args ...string) {
	t.Helper()
	full := append([]string{"new-session", "-d", "-s", name}, args...)
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("creating session %q: %v: %s", name, err, out)
	}
}

func TestListLiveSessions(t *testing.T) {
	isolate(t)
	newSession(t, "alpha")
	newSession(t, "beta")

	got, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got), got)
	}

	names := map[string]bool{}
	for _, s := range got {
		names[s.Name] = true
		if s.Command == "" {
			t.Errorf("session %q has no pane command; annotatePanes did not run", s.Name)
		}
		if s.Path == "" {
			t.Errorf("session %q has no pane path", s.Name)
		}
	}
	if !names["alpha"] || !names["beta"] {
		t.Errorf("missing expected sessions, got %v", names)
	}
}

// No tmux server is the ordinary state of a fresh machine, and it must read as
// an empty list rather than an error the UI has to special-case.
func TestListWithNoServer(t *testing.T) {
	isolate(t)

	got, err := List(context.Background())
	if err != nil {
		t.Fatalf("List with no server returned an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d sessions, want 0", len(got))
	}
}

// The end-to-end path: attach over a PTY, type a command, see its output.
func TestAttachRoundTrip(t *testing.T) {
	isolate(t)
	newSession(t, "round-trip")

	att, err := Open(context.Background(), "round-trip", Size{Cols: 80, Rows: 24}, ModeFit, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer att.Close()

	rec := record(att)

	// Let tmux paint its initial screen, then discard it, so the marker cannot
	// be matched against the redraw of the prompt.
	if first := rec.settle(750 * time.Millisecond); len(first) == 0 {
		t.Fatal("attach produced no initial screen; tmux did not attach")
	}
	rec.reset()

	if _, err := att.Write([]byte("echo TMUX-ROUNDTRIP-OK\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := rec.settle(2 * time.Second)
	if !strings.Contains(out, "TMUX-ROUNDTRIP-OK") {
		t.Fatalf("marker not found in terminal output; got %q", clip(out))
	}
}

// A read-only attach must not be able to run a command. This is the capability
// model's load-bearing test: enforcement is tmux's read-only client flag, not
// our own message handling, and this proves the flag is actually reaching tmux.
func TestReadOnlyAttachCannotType(t *testing.T) {
	isolate(t)
	newSession(t, "locked")

	att, err := Open(context.Background(), "locked", Size{Cols: 80, Rows: 24}, ModeFit, false)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer att.Close()

	rec := record(att)

	// Prove the attach is live before concluding anything from the absence of
	// the marker. Without this the test passes when attach is broken and emits
	// nothing at all -- which is exactly how it passed while the round-trip
	// test was failing.
	if first := rec.settle(750 * time.Millisecond); len(first) == 0 {
		t.Fatal("read-only attach produced no output; the test cannot distinguish enforcement from a broken attach")
	}

	if _, err := att.Write([]byte("echo SHOULD-NOT-RUN\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	out := rec.settle(2 * time.Second)

	// The echoed keystrokes must not appear, and neither must a result: a
	// read-only client's input is discarded by tmux before it reaches the pane.
	if strings.Contains(out, "SHOULD-NOT-RUN") {
		t.Fatalf("read-only attach accepted input; got %q", clip(out))
	}
}

func TestOpenNonexistentSession(t *testing.T) {
	isolate(t)
	newSession(t, "real")

	if _, err := Open(context.Background(), "not-real", Size{Cols: 80, Rows: 24}, ModeFit, true); err == nil {
		t.Fatal("Open succeeded for a session that does not exist; it must never create one")
	}
}

func TestOpenRejectsZeroSize(t *testing.T) {
	isolate(t)
	newSession(t, "sized")

	if _, err := Open(context.Background(), "sized", Size{}, ModeFit, true); err == nil {
		t.Fatal("Open accepted a zero terminal size")
	}
}

// recorder is the single reader for an attach.
//
// One reader, for the whole test. An earlier version started a fresh goroutine
// per read window and left each one running, so the first window's goroutine
// went on consuming output and swallowed the marker the second was waiting for
// -- a test that reported a product bug that did not exist.
type recorder struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func record(att *Attach) *recorder {
	r := &recorder{}
	go func() {
		b := make([]byte, 4096)
		for {
			n, err := att.Read(b)
			if n > 0 {
				r.mu.Lock()
				r.buf.Write(b[:n])
				r.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	return r
}

// settle waits for output to arrive and returns everything seen so far.
func (r *recorder) settle(d time.Duration) string {
	time.Sleep(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf.Reset()
}

func clip(s string) string {
	s = strings.ReplaceAll(s, "\x1b", "^[")
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// The phone attaches to a throwaway grouped view, not to the session itself.
// The view must never appear in the list, and killing it must leave the real
// session and everything running in it alone.
func TestPhoneViewIsHiddenAndDisposable(t *testing.T) {
	isolate(t)
	newSession(t, "work")

	att, err := Open(context.Background(), "work", Size{Cols: 80, Rows: 24}, ModeFit, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := record(att)
	if first := rec.settle(750 * time.Millisecond); len(first) == 0 {
		t.Fatal("attach produced no output")
	}

	// The view exists in tmux while attached...
	raw, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	if !strings.Contains(string(raw), ViewName("work")) {
		t.Fatalf("no phone view was created; tmux has: %q", raw)
	}

	// ...but the API never shows it.
	listed, err := List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, s := range listed {
		if IsView(s.Name) {
			t.Errorf("a phone view leaked into the session list: %q", s.Name)
		}
	}
	if len(listed) != 1 || listed[0].Name != "work" {
		t.Fatalf("got %d sessions, want just \"work\": %+v", len(listed), listed)
	}

	att.Close()
	time.Sleep(500 * time.Millisecond)

	// The view is gone, the real session is not.
	raw, _ = exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(raw), ViewName("work")) {
		t.Errorf("the phone view outlived the attach: %q", raw)
	}
	if !strings.Contains(string(raw), "work") {
		t.Fatalf("closing the phone view killed the real session; tmux has: %q", raw)
	}
}

func TestCreateAndKill(t *testing.T) {
	isolate(t)
	newSession(t, "anchor") // keeps the server alive between the two calls

	if err := Create(context.Background(), "made-by-api", t.TempDir()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ok, _ := Exists(context.Background(), "made-by-api")
	if !ok {
		t.Fatal("Create did not produce a session")
	}

	// Creating the same name twice must be an error, not a silent attach to
	// someone else's session.
	if err := Create(context.Background(), "made-by-api", ""); err == nil {
		t.Fatal("Create accepted a duplicate name")
	}

	if err := Kill(context.Background(), "made-by-api"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	ok, _ = Exists(context.Background(), "made-by-api")
	if ok {
		t.Fatal("Kill left the session running")
	}
	if err := Kill(context.Background(), "made-by-api"); err == nil {
		t.Fatal("Kill of a missing session should be an error")
	}
}

func TestValidName(t *testing.T) {
	bad := []string{"", "  ", " leading", "trailing ", "has.dot", "has:colon", "x~phone", strings.Repeat("a", 65)}
	for _, name := range bad {
		if err := ValidName(name); err == nil {
			t.Errorf("ValidName(%q) accepted a name tmux cannot address", name)
		}
	}
	for _, name := range []string{"work", "my-project", "chem check", "agent_1"} {
		if err := ValidName(name); err != nil {
			t.Errorf("ValidName(%q) rejected a reasonable name: %v", name, err)
		}
	}
}

// Killing a session must take its view with it. tmux keeps a session group
// alive through any member, so leaving the view behind makes the session look
// like it survived being killed.
func TestKillRemovesTheView(t *testing.T) {
	isolate(t)
	newSession(t, "anchor")
	newSession(t, "doomed")

	att, err := Open(context.Background(), "doomed", Size{Cols: 80, Rows: 24}, ModeFit, true)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	record(att).settle(500 * time.Millisecond)

	if err := Kill(context.Background(), "doomed"); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	att.Close()

	raw, _ := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if strings.Contains(string(raw), "doomed") {
		t.Fatalf("the session survived being killed: %q", raw)
	}
}
