// Package session enumerates and attaches to tmux sessions.
//
// tmux is the transport for everything the daemon serves. It already solves
// detach and reattach, and the session someone opens from a phone is the same
// one on the monitor at their desk, which is the whole point.
package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// sep separates format fields. ASCII unit separator is used rather than a tab
// or a pipe because a tmux session name may legally contain both, and a name
// containing the separator would otherwise shift every later field by one.
const sep = "\x1f"

// listFormat must stay in sync with parseSession.
const listFormat = "#{session_name}" + sep +
	"#{session_windows}" + sep +
	"#{session_attached}" + sep +
	"#{session_created}" + sep +
	"#{session_activity}"

// paneFormat collects the per-session detail shown in the list. The active
// pane's command is what agent detection reads in step 5.
const paneFormat = "#{session_name}" + sep +
	"#{pane_current_command}" + sep +
	"#{pane_current_path}"

// Session is one tmux session as the API reports it.
type Session struct {
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Attached bool      `json:"attached"`
	Created  time.Time `json:"created"`
	Activity time.Time `json:"activity"`

	// Command and Path describe the active pane of the active window: what is
	// running, and where. Empty when tmux reports no pane, which happens in the
	// moment between a session being created and its first pane existing.
	Command string `json:"command,omitempty"`
	Path    string `json:"path,omitempty"`
}

// ErrNoServer means no tmux server is running, which is not a failure: it is
// the ordinary state of a machine with no sessions open.
var ErrNoServer = errors.New("no tmux server running")

// List returns every tmux session, newest activity first.
func List(ctx context.Context) ([]Session, error) {
	out, err := run(ctx, "list-sessions", "-F", listFormat)
	if err != nil {
		if errors.Is(err, ErrNoServer) {
			// An empty list, not an error. A phone opening the app before any
			// session exists should see "no sessions", not a red banner.
			return nil, nil
		}
		return nil, err
	}

	var sessions []Session
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		s, err := parseSession(line)
		if err != nil {
			return nil, err
		}
		// Phone views are ours, not the user's. Hidden from the list, and
		// collected here if one outlived the daemon that made it -- a stray
		// view holds a tmux client and keeps voting on window size.
		if IsView(s.Name) {
			if !s.Attached {
				_, _ = run(ctx, "kill-session", "-t", s.Name)
			}
			continue
		}
		sessions = append(sessions, s)
	}

	if err := annotatePanes(ctx, sessions); err != nil {
		// Pane detail is decoration. A session list without it is still useful,
		// so this never fails the request.
		_ = err
	}

	return sessions, nil
}

func parseSession(line string) (Session, error) {
	f := strings.Split(line, sep)
	if len(f) != 5 {
		return Session{}, fmt.Errorf("tmux returned %d fields, want 5: %q", len(f), line)
	}

	windows, err := strconv.Atoi(f[1])
	if err != nil {
		return Session{}, fmt.Errorf("parsing window count %q: %w", f[1], err)
	}

	return Session{
		Name:     f[0],
		Windows:  windows,
		Attached: f[2] != "0",
		Created:  unixOrZero(f[3]),
		Activity: unixOrZero(f[4]),
	}, nil
}

// annotatePanes fills Command and Path from the active pane of each session.
func annotatePanes(ctx context.Context, sessions []Session) error {
	// -a lists panes across every session in one call rather than one exec per
	// session; the filter keeps only the active pane of the active window, so
	// each session contributes exactly one line.
	out, err := run(ctx, "list-panes", "-a",
		"-f", "#{&&:#{window_active},#{pane_active}}",
		"-F", paneFormat)
	if err != nil {
		return err
	}

	byName := make(map[string]*Session, len(sessions))
	for i := range sessions {
		byName[sessions[i].Name] = &sessions[i]
	}

	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		f := strings.Split(line, sep)
		if len(f) != 3 {
			continue
		}
		if s, ok := byName[f[0]]; ok {
			s.Command = f[1]
			s.Path = f[2]
		}
	}
	return nil
}

// Exists reports whether a session of that exact name is present.
//
// Callers must use this before attaching rather than letting tmux decide: a
// name that does not exist would otherwise be created on attach in some
// invocations, and silently conjuring a session because a URL had a typo in it
// is not a behaviour anyone wants.
func Exists(ctx context.Context, name string) (bool, error) {
	sessions, err := List(ctx)
	if err != nil {
		return false, err
	}
	for _, s := range sessions {
		if s.Name == name {
			return true, nil
		}
	}
	return false, nil
}

func run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "tmux", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr := strings.TrimSpace(string(ee.Stderr))
			// tmux says "no server running" when the socket directory exists
			// and "error connecting to ... (No such file or directory)" when it
			// does not -- the state of a machine that has never started tmux.
			// Both mean the same thing to a caller, and missing the second one
			// shows a fresh install an error where it should see an empty list.
			if strings.Contains(stderr, "no server running") ||
				strings.Contains(stderr, "No such file or directory") {
				return "", ErrNoServer
			}
			if stderr != "" {
				return "", fmt.Errorf("tmux %s: %s", args[0], stderr)
			}
		}
		return "", fmt.Errorf("tmux %s: %w", args[0], err)
	}
	return string(out), nil
}

func unixOrZero(s string) time.Time {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

// Create starts a new tmux session.
//
// This exists so that nobody has to know tmux to get a session going. The name
// is validated rather than passed through: tmux treats "." and ":" as target
// separators, so a name containing them refers to something other than itself
// and produces errors that make no sense to whoever typed it.
func Create(ctx context.Context, name, dir string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if dir == "" {
		dir = os.Getenv("HOME")
	}

	exists, err := Exists(ctx, name)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("a session named %q already exists", name)
	}

	if _, err := run(ctx, "new-session", "-d", "-s", name, "-c", dir); err != nil {
		return err
	}

	// tmux can fail to start its server and still exit 0 -- it does exactly
	// that when it cannot create its socket, which is what a sandboxed unit
	// with a read-only /tmp produces. Without this check the daemon reports a
	// session it did not create and the caller is left staring at an empty
	// list, with a success in the log.
	created, err := Exists(ctx, name)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("tmux reported success but no session %q exists; "+
			"the daemon may not be able to write to /tmp", name)
	}
	return nil
}

// Kill ends a session and everything running in it.
func Kill(ctx context.Context, name string) error {
	if IsView(name) {
		return errors.New("that is an internal view, not a session")
	}
	exists, err := Exists(ctx, name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("no tmux session named %q", name)
	}

	// Take the view with it, or tmux keeps the group alive through the view and
	// the session appears to survive being killed.
	_, _ = run(ctx, "kill-session", "-t", ViewName(name))
	_, err = run(ctx, "kill-session", "-t", name)
	return err
}

// ValidName rejects names tmux cannot address unambiguously.
func ValidName(name string) error {
	switch {
	case name == "":
		return errors.New("a session needs a name")
	case len(name) > 64:
		return errors.New("that name is too long")
	case strings.ContainsAny(name, ".:"):
		return errors.New("a session name cannot contain '.' or ':'")
	case strings.TrimSpace(name) != name:
		return errors.New("a session name cannot start or end with a space")
	case IsView(name):
		return errors.New("that name is reserved")
	}
	return nil
}
