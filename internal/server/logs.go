// Logs endpoint — serves recent log output at /logs/.
//
// Uses an in-memory ring buffer that captures slog output as it's written.
// The buffer is wrapped around the real slog handler so all logging
// infrastructure works as normal.
package server

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"sync"
)

// ringBufferCapacity is the number of log lines kept in memory.
const ringBufferCapacity = 1000

// ringBufferHandler is an slog.Handler that writes to an in-memory ring
// buffer in addition to delegating to the wrapped handler.
type ringBufferHandler struct {
	inner slog.Handler
	mu    sync.Mutex
	buf   [ringBufferCapacity]string
	pos   int
	full  bool
}

// NewRingBufferHandler wraps an slog.Handler with a ring buffer.
func NewRingBufferHandler(inner slog.Handler) *ringBufferHandler {
	return &ringBufferHandler{inner: inner}
}

// Enabled reports whether the handler is enabled for the given level.
func (h *ringBufferHandler) Enabled(_ context.Context, level slog.Level) bool {
	return h.inner.Enabled(context.Background(), level)
}

// Handle stores the formatted log line in the ring buffer, then delegates
// to the wrapped handler.
func (h *ringBufferHandler) Handle(_ context.Context, r slog.Record) error {
	line := formatRecord(&r)
	h.mu.Lock()
	h.buf[h.pos] = line
	h.pos = (h.pos + 1) % ringBufferCapacity
	if h.pos == 0 {
		h.full = true
	}
	h.mu.Unlock()
	return h.inner.Handle(context.Background(), r)
}

// WithAttrs returns a new handler with the given attributes.
func (h *ringBufferHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &ringBufferHandler{
		inner: h.inner.WithAttrs(attrs),
		pos:   h.pos,
		full:  h.full,
		buf:   h.buf,
	}
}

// WithGroup returns a new handler with the given group.
func (h *ringBufferHandler) WithGroup(name string) slog.Handler {
	return &ringBufferHandler{
		inner: h.inner.WithGroup(name),
		pos:   h.pos,
		full:  h.full,
		buf:   h.buf,
	}
}

// Lines returns the most recent N log lines in reverse chronological order.
func (h *ringBufferHandler) Lines(n int) []string {
	h.mu.Lock()
	defer h.mu.Unlock()

	if n <= 0 || n > ringBufferCapacity {
		n = ringBufferCapacity
	}

	count := h.pos
	if h.full {
		count = ringBufferCapacity
	}
	if n > count {
		n = count
	}

	lines := make([]string, n)
	for i := 0; i < n; i++ {
		idx := (h.pos - 1 - i + ringBufferCapacity) % ringBufferCapacity
		if !h.full && idx < 0 {
			break
		}
		lines[i] = h.buf[idx]
	}
	return lines
}

// formatRecord formats a slog.Record as a single-line string similar to
// the text handler.
func formatRecord(r *slog.Record) string {
	timeStr := r.Time.Format("2006-01-02T15:04:05.000Z07:00")
	levelStr := r.Level.String()
	msg := r.Message

	line := fmt.Sprintf("%s %-5s %s", timeStr, levelStr, msg)

	r.Attrs(func(a slog.Attr) bool {
		if a.Value.Kind() == slog.KindGroup {
			return true
		}
		line += fmt.Sprintf(" %s=%v", a.Key, a.Value.Any())
		return true
	})

	return line
}

// globalLogBuffer stores the ring buffer reference for the server.
var globalLogBuffer *ringBufferHandler

// SetLogBuffer sets the global ring buffer reference so the /logs/
// handler can access it.
func SetLogBuffer(buf *ringBufferHandler) {
	globalLogBuffer = buf
}

// handleLogs serves recent log output as HTML.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	fmt.Fprint(w, logsHTMLStart)
	fmt.Fprint(w, "</head><body>\n")
	fmt.Fprint(w, "<div class=\"container\">\n")
	fmt.Fprintf(w, "<h1>warpbox <span class=\"path\">logs</span></h1>\n")
	fmt.Fprint(w, "<p class=\"nav\"><a href=\"/\">Back to status</a></p>\n")
	fmt.Fprint(w, "<pre class=\"log-output\">")

	var lines []string
	if globalLogBuffer != nil {
		lines = globalLogBuffer.Lines(200)
	}

	for _, line := range lines {
		fmt.Fprintln(w, html.EscapeString(line))
	}

	fmt.Fprint(w, "</pre>\n")
	fmt.Fprint(w, "</div>\n")
	fmt.Fprint(w, "</body></html>\n")
}

const logsHTMLStart = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<style>
  * { margin: 0; padding: 0; box-sizing: border-box; }
  body {
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
    background: #0f172a;
    color: #e2e8f0;
    padding: 2rem 1rem;
  }
  .container { max-width: 1000px; margin: 0 auto; }
  h1 { font-size: 1.5rem; color: #38bdf8; margin-bottom: 0.5rem; }
  h1 .path { color: #94a3b8; font-weight: 400; }
  .nav { margin-bottom: 1rem; font-size: 0.85rem; }
  .nav a { color: #38bdf8; text-decoration: none; }
  .nav a:hover { text-decoration: underline; }
  .log-output {
    background: #1e293b;
    color: #e2e8f0;
    padding: 1rem;
    border-radius: 8px;
    font-family: "SF Mono", "Fira Code", "Consolas", monospace;
    font-size: 0.8rem;
    line-height: 1.5;
    overflow-x: auto;
    white-space: pre-wrap;
    max-height: 80vh;
    overflow-y: auto;
  }
</style>
<title>warpbox logs</title>
`