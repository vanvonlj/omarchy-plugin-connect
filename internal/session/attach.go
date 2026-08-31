package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

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

// Attach is a live connection to a tmux session, backed by a PTY.
type Attach struct {
	pty  *os.File
	cmd  *exec.Cmd
	name string
}

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

	// -t targets an existing session and never creates one. new-session -A
	// would be friendlier for a typo and much worse for a shared machine.
	args := []string{"attach-session", "-t", name}
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

	return &Attach{pty: f, cmd: cmd, name: name}, nil
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
	err := a.pty.Close()
	if a.cmd.Process != nil {
		a.cmd.Process.Kill()
	}
	// Reap the child so a phone reconnecting through a flaky network does not
	// accumulate zombie tmux clients.
	a.cmd.Wait()
	return err
}
