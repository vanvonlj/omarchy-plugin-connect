package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/creack/pty"
)

// Size is a terminal size in character cells.
type Size struct {
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

func (s Size) valid() bool {
	return s.Cols > 0 && s.Rows > 0
}

// Mode controls how an attaching client interacts with other clients on the
// same session.
type Mode string

const (
	// ModeFit lets this client's size drive the tmux window, so the terminal
	// fits the phone. tmux's window-size option defaults to "latest", so a
	// desktop client reclaims the size as soon as it is used again -- which is
	// what makes this safe as a default rather than a lasting disruption.
	ModeFit Mode = "fit"

	// ModeMirror attaches with ignore-size, so this client never affects the
	// window size other clients see. The phone may then see a window wider than
	// its screen. This is the mode for watching a session someone else is
	// actively using.
	ModeMirror Mode = "mirror"
)

// viewSuffix marks the throwaway session created for a phone.
//
// Attaching to the real session directly means inheriting its options, and the
// two that matter most cannot be changed without changing them for the desktop
// too: the status bar, which is the single loudest "you are looking at tmux"
// signal, and mouse mode, without which there is no way to scroll on a
// touchscreen at all.
//
// A grouped session shares the same windows -- the same panes, the same running
// agent -- while keeping its own options and its own current window. So the
// phone gets a clean view of exactly what the desktop is showing.
const viewSuffix = "~phone"

// Attach is a live connection to a tmux session, backed by a PTY.
type Attach struct {
	pty  *os.File
	cmd  *exec.Cmd
	name string
	once sync.Once

	// view is the throwaway grouped session, killed on Close. Empty when
	// attaching directly.
	view string
}

// ViewName returns the phone-view session name for a target.
func ViewName(target string) string { return target + viewSuffix }

// IsView reports whether a session is one of our throwaway phone views. The
// listing hides these: they are an implementation detail, and showing someone a
// second copy of their own session called "work~phone" is a good way to make
// them think something is broken.
func IsView(name string) bool { return strings.HasSuffix(name, viewSuffix) }

// Open attaches to an existing tmux session.
//
// writable=false attaches with tmux's own read-only client flag rather than by
// declining to forward keystrokes in our code. Enforcement belongs as close to
// the terminal as it can get: a bug in the daemon's message handling then
// cannot turn a read-only device into a writable one.
func Open(ctx context.Context, name string, size Size, mode Mode, writable bool) (*Attach, error) {
	exists, err := Exists(ctx, name)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, fmt.Errorf("no tmux session named %q", name)
	}
	if !size.valid() {
		return nil, fmt.Errorf("invalid terminal size %dx%d", size.Cols, size.Rows)
	}

	flags := clientFlags(mode, writable)

	// Build the phone's own view of the session. A failure here is not fatal:
	// attaching directly still works, it just looks like tmux.
	view := ViewName(name)
	target := name
	if err := createView(ctx, name, view); err == nil {
		target = view
	} else {
		view = ""
	}

	// -t targets an existing session and never creates one. new-session -A
	// would be friendlier for a typo and much worse for a shared machine.
	args := []string{"attach-session", "-t", target}
	if len(flags) > 0 {
		args = append(args, "-f", strings.Join(flags, ","))
	}

	cmd := exec.Command("tmux", args...)

	// tmux refuses to attach without a controlling terminal, so the PTY is not
	// an implementation detail we could skip: it is what makes attach possible.
	//
	// SysProcAttr is deliberately left alone. pty.StartWithSize sets Setsid,
	// Setctty *and* the Ctty file descriptor together; supplying our own
	// Setsid/Setctty without the matching Ctty leaves the child with no
	// controlling terminal, and tmux then attaches to nothing and emits no
	// output at all -- a silent failure rather than an error.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	f, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: size.Cols,
		Rows: size.Rows,
	})
	if err != nil {
		return nil, fmt.Errorf("attaching to %q: %w", name, err)
	}

	return &Attach{pty: f, cmd: cmd, name: name, view: view}, nil
}

// createView makes the grouped session the phone attaches to.
//
// destroy-unattached would be the obvious way to guarantee cleanup, and it
// cannot be used here: setting it on a session that has no client yet destroys
// that session immediately, before anything can attach. Cleanup is therefore
// Close, plus the sweep in List for views that outlived their daemon.
func createView(ctx context.Context, target, view string) error {
	// Kill any stale view first. One can survive a daemon that was killed
	// rather than shut down, and attaching to it would give the phone whatever
	// window that dead session was last looking at.
	_, _ = run(ctx, "kill-session", "-t", view)

	if _, err := run(ctx, "new-session", "-d", "-t", target, "-s", view); err != nil {
		return err
	}

	// Options are set on the view only, so the desktop keeps its own. Errors
	// are ignored individually: a view with a status bar is worse-looking, not
	// broken, and is not worth failing an attach over.
	for _, opt := range [][]string{
		// The status bar is tmux's most recognisable feature and the least
		// useful on a four-inch screen.
		{"status", "off"},
		// Without mouse mode there is no scrolling on a touchscreen at all --
		// history would be reachable only through copy-mode key chords.
		{"mouse", "on"},
	} {
		_, _ = run(ctx, append([]string{"set-option", "-t", view}, opt...)...)
	}
	return nil
}

func clientFlags(mode Mode, writable bool) []string {
	var flags []string
	if mode == ModeMirror {
		flags = append(flags, "ignore-size")
	}
	if !writable {
		flags = append(flags, "read-only")
	}
	return flags
}

// Read returns terminal output.
func (a *Attach) Read(p []byte) (int, error) { return a.pty.Read(p) }

// Write sends input to the terminal.
func (a *Attach) Write(p []byte) (int, error) { return a.pty.Write(p) }

// Resize changes the PTY size, which tmux reads as a client resize.
func (a *Attach) Resize(size Size) error {
	if !size.valid() {
		return fmt.Errorf("invalid terminal size %dx%d", size.Cols, size.Rows)
	}
	return pty.Setsize(a.pty, &pty.Winsize{Cols: size.Cols, Rows: size.Rows})
}

// Close detaches this client. The tmux session keeps running: detaching is the
// normal end of a connection here, not a teardown.
func (a *Attach) Close() error {
	// Idempotent: the handler defers this, and the cancel path calls it to
	// unblock a PTY read. Both must be safe.
	var err error
	a.once.Do(func() {
		err = a.pty.Close()
		if a.cmd.Process != nil {
			a.cmd.Process.Kill()
		}
		if a.view != "" {
			// The view is disposable by design; the session it mirrors is not.
			_, _ = run(context.Background(), "kill-session", "-t", a.view)
		}
		// Reap the child so a phone reconnecting through a flaky network does
		// not accumulate zombie tmux clients.
		a.cmd.Wait()
	})
	return err
}
