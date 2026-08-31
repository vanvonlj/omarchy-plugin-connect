package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/vanvonlj/omarchy-plugin-connect/internal/session"
)

// attachWriteTimeout bounds a single frame write to a client.
//
// A phone that walks into a lift stops reading without closing, and an
// unbounded write would then pin a PTY reader and a tmux client forever. The
// terminal is the wrong place to wait patiently.
const attachWriteTimeout = 10 * time.Second

// outputBuf is the PTY read size. Large enough that a screen redraw is one or
// two frames, small enough that an idle session is not holding much.
const outputBuf = 32 * 1024

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := session.List(r.Context())
	if err != nil {
		s.log.Error("listing sessions", "err", err)
		http.Error(w, "could not list sessions", http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		// Encode an empty array rather than null: the client should not have to
		// special-case "no tmux server running" as a different shape.
		sessions = []session.Session{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(sessions)
}

// control is a client message on the text channel. Terminal input travels as
// binary frames instead, so no encoding or escaping sits between a keystroke
// and the PTY.
type control struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

func (s *Server) handleAttach(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	size := session.Size{
		Cols: uint16(queryInt(r, "cols", 80)),
		Rows: uint16(queryInt(r, "rows", 24)),
	}

	mode := session.ModeFit
	if r.URL.Query().Get("mode") == string(session.ModeMirror) {
		mode = session.ModeMirror
	}

	// Every attach is writable for now. Per-device capabilities arrive with
	// pairing in step 3; until then the only callers are tailnet peers that
	// already passed admission, and the plumbing below takes the flag so that
	// change is a one-line edit rather than a redesign.
	writable := true

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only, which is the default, stated here because it is a
		// security decision rather than an omission. The PWA is served from
		// this origin; nothing else has business opening a terminal.
		OriginPatterns: []string{},
	})
	if err != nil {
		s.log.Warn("websocket upgrade failed", "err", err)
		return
	}
	defer conn.CloseNow()

	att, err := session.Open(r.Context(), name, size, mode, writable)
	if err != nil {
		s.log.Warn("attach failed", "session", name, "err", err)
		conn.Close(websocket.StatusPolicyViolation, "cannot attach")
		return
	}
	defer att.Close()

	s.log.Info("attached", "session", name, "mode", mode, "size", size)

	// The two pumps are peers: whichever stops first tears down the other by
	// cancelling this context, so a dead PTY does not leave a websocket open
	// and a closed websocket does not leave a tmux client attached.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	go s.pumpInput(ctx, cancel, conn, att)
	s.pumpOutput(ctx, cancel, conn, att)

	s.log.Info("detached", "session", name)
}

// pumpInput carries client frames to the PTY.
func (s *Server) pumpInput(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, att *session.Attach) {
	defer cancel()

	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}

		switch typ {
		case websocket.MessageBinary:
			if _, err := att.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var c control
			if err := json.Unmarshal(data, &c); err != nil {
				// A malformed control frame is ignored rather than fatal: it
				// must never be possible to drop someone's terminal by sending
				// one bad message.
				s.log.Debug("ignoring malformed control frame", "err", err)
				continue
			}
			if c.Type == "resize" {
				if err := att.Resize(session.Size{Cols: c.Cols, Rows: c.Rows}); err != nil {
					s.log.Debug("ignoring bad resize", "err", err)
				}
			}
		}
	}
}

// pumpOutput carries PTY output to the client.
func (s *Server) pumpOutput(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, att *session.Attach) {
	defer cancel()

	buf := make([]byte, outputBuf)
	for {
		n, err := att.Read(buf)
		if n > 0 {
			writeCtx, done := context.WithTimeout(ctx, attachWriteTimeout)
			err := conn.Write(writeCtx, websocket.MessageBinary, buf[:n])
			done()
			if err != nil {
				return
			}
		}
		if err != nil {
			// io.EOF is the ordinary end of an attach: the session was killed,
			// or the client detached. Close cleanly so the browser can tell
			// that apart from a network drop and decide whether to reconnect.
			if errors.Is(err, io.EOF) {
				conn.Close(websocket.StatusNormalClosure, "session ended")
			}
			return
		}
	}
}

func queryInt(r *http.Request, key string, fallback int) int {
	v := r.URL.Query().Get(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 1000 {
		return fallback
	}
	return n
}
