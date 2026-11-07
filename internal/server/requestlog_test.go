package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/y0f/asura/internal/storage"
)

func TestClassifyRoute(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/", "web"},
		{"/monitors", "web"},
		{"/monitors/1", "web"},
		{"/incidents", "web"},
		{"/logs", "web"},
		{"/api/v1/monitors", "api"},
		{"/api/v1/monitors/1", "api"},
		{"/api/v1/incidents", "api"},
		{"/api/v1/badge/1/status", "badge"},
		{"/api/v1/badge/1/uptime", "badge"},
		{"/login", "auth"},
		{"/logout", "auth"},
		{"/metrics", "metrics"},
		{"/api/v1/heartbeat/sha256:0123456789ab", "heartbeat"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := classifyRoute(tt.path)
			if got != tt.want {
				t.Errorf("classifyRoute(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestExtractMonitorID(t *testing.T) {
	tests := []struct {
		path string
		want int64
	}{
		{"/monitors/4", 4},
		{"/monitors/123", 123},
		{"/api/v1/monitors/7", 7},
		{"/api/v1/monitors/7/checks", 7},
		{"/api/v1/badge/42/status", 42},
		{"/api/v1/badge/42/uptime", 42},
		{"/", 0},
		{"/monitors", 0},
		{"/api/v1/monitors", 0},
		{"/incidents/5", 0},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractMonitorID(tt.path)
			if got != tt.want {
				t.Errorf("extractMonitorID(%q) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

func TestShouldSkipLog(t *testing.T) {
	tests := []struct {
		path     string
		basePath string
		want     bool
	}{
		{"/static/htmx.min.js", "", true},
		{"/static/logo.gif", "", true},
		{"/api/v1/health", "", true},
		{"/myapp/static/htmx.min.js", "/myapp", true},
		{"/myapp/api/v1/health", "/myapp", true},
		{"/", "", false},
		{"/monitors", "", false},
		{"/api/v1/monitors", "", false},
		{"/logs", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := shouldSkipLog(tt.path, tt.basePath)
			if got != tt.want {
				t.Errorf("shouldSkipLog(%q, %q) = %v, want %v", tt.path, tt.basePath, got, tt.want)
			}
		})
	}
}

func TestTruncate(t *testing.T) {
	if truncate("hello", 10) != "hello" {
		t.Error("short string should not be truncated")
	}
	if truncate("hello world", 5) != "hello" {
		t.Error("long string should be truncated to maxLen")
	}
	if truncate("", 5) != "" {
		t.Error("empty string should stay empty")
	}
}

func TestRequestLogRedactsHeartbeatToken(t *testing.T) {
	const token = "abcdef0123456789abcdef0123456789"
	digest := "sha256:" + storage.HashToken(token)[:heartbeatDigestLen]

	tests := []struct {
		name     string
		basePath string
		method   string
	}{
		{"no base path GET", "", http.MethodGet},
		{"no base path POST", "", http.MethodPost},
		{"base path GET", "/asura", http.MethodGet},
		{"base path POST", "/asura", http.MethodPost},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &RequestLogWriter{ch: make(chan *storage.RequestLog, 1)}
			h := requestLogMiddleware(writer, tt.basePath, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

			req := httptest.NewRequest(tt.method, tt.basePath+"/api/v1/heartbeat/"+token, nil)
			h.ServeHTTP(httptest.NewRecorder(), req)

			var log *storage.RequestLog
			select {
			case log = <-writer.ch:
			default:
				t.Fatal("no request log written")
			}
			if strings.Contains(log.Path, token) || strings.Contains(log.Path, token[:8]) {
				t.Fatalf("logged path %q contains the heartbeat token", log.Path)
			}
			if want := tt.basePath + "/api/v1/heartbeat/" + digest; log.Path != want {
				t.Fatalf("logged path = %q, want %q", log.Path, want)
			}
			if log.RouteGroup != "heartbeat" {
				t.Fatalf("route group = %q, want heartbeat", log.RouteGroup)
			}
			// The ping handler still saw the real path.
			if req.URL.Path != tt.basePath+"/api/v1/heartbeat/"+token {
				t.Fatalf("request path was modified: %q", req.URL.Path)
			}
		})
	}
}

func TestRedactHeartbeatPath(t *testing.T) {
	// Pings from one monitor share a digest; different tokens differ.
	a1, _ := redactHeartbeatPath("/api/v1/heartbeat/token-a")
	a2, _ := redactHeartbeatPath("/api/v1/heartbeat/token-a")
	b, _ := redactHeartbeatPath("/api/v1/heartbeat/token-b")
	if a1 != a2 || a1 == b {
		t.Fatalf("digests not stable per token: %q %q %q", a1, a2, b)
	}
	// The digest is a prefix of the at-rest hash in heartbeats.token.
	if !strings.HasPrefix(storage.HashToken("token-a"), strings.TrimPrefix(a1, "/api/v1/heartbeat/sha256:")) {
		t.Fatalf("%q is not a prefix of the stored token hash", a1)
	}
	for _, p := range []string{"/api/v1/heartbeat/", "/api/v1/monitors/1", "/status/heartbeat"} {
		if got, ok := redactHeartbeatPath(p); ok || got != p {
			t.Errorf("redactHeartbeatPath(%q) = %q, %v; want unchanged", p, got, ok)
		}
	}
}
