package server

import (
	"net/http"
	"time"
)

// statusRecorder captures the status code so the access log can report it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer, which the
// websocket hijack needs. Without it, wrapping the writer breaks every attach.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// accessLog records every request.
//
// Debug level, so it is off by default: a terminal generates a lot of traffic
// and nobody needs a line per keystroke. It exists because "the phone shows a
// white screen" is otherwise undiagnosable from this side -- a successful GET
// of a static file leaves no trace at all, so silence in the log could mean
// either "nothing arrived" or "everything worked".
func (s *Server) accessLog(tier string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.log.Enabled(r.Context(), -4) { // slog.LevelDebug
			next.ServeHTTP(w, r)
			return
		}

		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)

		s.log.Debug("request",
			"tier", tier,
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery != "",
			"status", rec.status,
			"bytes", rec.bytes,
			"remote", r.RemoteAddr,
			"ua", r.UserAgent(),
			"took", time.Since(start).Round(time.Millisecond).String(),
		)
	})
}
