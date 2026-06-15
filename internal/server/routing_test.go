package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ben/warpbox/internal/metadata"
	"github.com/ben/warpbox/internal/throttle"
)

func testServer(t *testing.T, overrides ...Config) *Server {
	store, err := metadata.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := Config{Version: "test", WebDAVRoot: "/webdav"}
	if len(overrides) > 0 {
		cfg = overrides[0]
	}

	queue := throttle.NewQueue(600)
	return New(cfg, store, nil, queue)
}

func TestRouteTable(t *testing.T) {
	srv := testServer(t)

	req := func(method, path string) *http.Response {
		r := httptest.NewRequest(method, path, nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		return w.Result()
	}

	tests := []struct {
		name     string
		method   string
		path     string
		wantCode int
	}{
		// Landing page
		{"landing root", http.MethodGet, "/", 200},

		// Health
		{"healthz", http.MethodGet, "/healthz", 200},

		// Stats
		{"stats.json", http.MethodGet, "/stats.json", 200},

		// WebDAV — exact and prefix
		{"webdav exact", http.MethodGet, "/webdav", http.StatusMultiStatus},
		{"webdav prefix", http.MethodGet, "/webdav/", http.StatusMultiStatus},

		// WebDAV method routing via internal dispatch
		{"webdav options", http.MethodOptions, "/webdav/", 200},
		{"webdav propfind", "PROPFIND", "/webdav/", http.StatusMultiStatus},
		{"webdav head missing", http.MethodHead, "/webdav/nonexistent", 404},
		{"webdav delete not allowed", http.MethodDelete, "/webdav/", 405},
		{"webdav post not allowed", http.MethodPost, "/webdav/", 405},

		// Infuse — exact and prefix
		{"infuse exact", http.MethodGet, "/infuse", http.StatusMultiStatus},
		{"infuse prefix", http.MethodGet, "/infuse/", http.StatusMultiStatus},

		// Infuse method routing via internal dispatch (path rewritten to s.root)
		{"infuse options", http.MethodOptions, "/infuse/", 200},
		{"infuse propfind", "PROPFIND", "/infuse/", http.StatusMultiStatus},
		{"infuse head missing", http.MethodHead, "/infuse/nonexistent", 404},
		{"infuse post not allowed", http.MethodPost, "/infuse/", 405},

		// HTTP browser
		{"http exact", http.MethodGet, "/http", 200},
		{"http prefix", http.MethodGet, "/http/", 200},

		// Logs — handled via exact match routes only
		{"logs exact", http.MethodGet, "/logs", 200},
		{"logs slash", http.MethodGet, "/logs/", 200},

		// Actions
		{"actions resync no config", http.MethodPost, "/actions/resync", 500},
		{"actions restart-sync no config", http.MethodPost, "/actions/restart-sync", 500},
		{"actions loglevel missing param", http.MethodPost, "/actions/loglevel", 400},
		{"actions unknown", http.MethodPost, "/actions/unknown", 404},

		// Method not allowed on actions (GET has no handler)
		{"actions get not allowed", http.MethodGet, "/actions/resync", 405},

		// Static assets
		{"warpbox svg", http.MethodGet, "/warpbox.svg", 200},
		{"favicon ico", http.MethodGet, "/favicon.ico", 200},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := req(tt.method, tt.path)
			defer resp.Body.Close()
			if resp.StatusCode != tt.wantCode {
				t.Errorf("%s %s: got status %d, want %d",
					tt.method, tt.path, resp.StatusCode, tt.wantCode)
			}
		})
	}
}

func TestRouteServerHeader(t *testing.T) {
	srv := testServer(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	resp := w.Result()
	defer resp.Body.Close()

	if got := resp.Header.Get("Server"); got != "warpbox/test" {
		t.Errorf("expected Server header 'warpbox/test', got %q", got)
	}
}

func TestRoute404(t *testing.T) {
	srv := testServer(t)

	paths := []string{
		"/nonexistent",
		"/logs/foobar",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, path, nil)
			w := httptest.NewRecorder()
			srv.mux.ServeHTTP(w, r)
			resp := w.Result()
			defer resp.Body.Close()
			if resp.StatusCode != 404 {
				t.Errorf("GET %s: got status %d, want 404", path, resp.StatusCode)
			}
		})
	}
}

func TestRouteInfuseRewrite(t *testing.T) {
	srv := testServer(t)

	// OPTIONS on /infuse/ should produce the same DAV/Allow headers as /webdav/.
	webdav := func() *http.Response {
		r := httptest.NewRequest(http.MethodOptions, "/webdav/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		return w.Result()
	}()
	defer webdav.Body.Close()

	infuse := func() *http.Response {
		r := httptest.NewRequest(http.MethodOptions, "/infuse/", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		return w.Result()
	}()
	defer infuse.Body.Close()

	if webdav.Header.Get("DAV") != infuse.Header.Get("DAV") {
		t.Errorf("DAV header mismatch: webdav=%q infuse=%q",
			webdav.Header.Get("DAV"), infuse.Header.Get("DAV"))
	}
	if webdav.Header.Get("Allow") != infuse.Header.Get("Allow") {
		t.Errorf("Allow header mismatch: webdav=%q infuse=%q",
			webdav.Header.Get("Allow"), infuse.Header.Get("Allow"))
	}
}

func TestRoutePprofDisabled(t *testing.T) {
	srv := testServer(t, Config{Version: "test", WebDAVRoot: "/webdav", EnablePprof: false})

	r := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, r)
	resp := w.Result()
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Errorf("expected 404 for disabled pprof, got %d", resp.StatusCode)
	}
}

func TestRoutePprofEnabled(t *testing.T) {
	srv := testServer(t, Config{Version: "test", WebDAVRoot: "/webdav", EnablePprof: true})

	// Verify exact path works (pprof handler may redirect).
	t.Run("exact", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/debug/pprof", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		resp := w.Result()
		defer resp.Body.Close()
		// pprof redirects /debug/pprof → /debug/pprof/ (307) or serves (200).
		if resp.StatusCode != 200 && resp.StatusCode != 307 {
			t.Errorf("/debug/pprof: got status %d, expected 200 or 307", resp.StatusCode)
		}
	})

	// Verify sub-path works.
	t.Run("subpath", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/debug/pprof/heap", nil)
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, r)
		resp := w.Result()
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("/debug/pprof/heap: got status %d, want 200", resp.StatusCode)
		}
	})
}
