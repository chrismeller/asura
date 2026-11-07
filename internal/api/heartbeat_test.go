package api_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/y0f/asura/internal/config"
	"github.com/y0f/asura/internal/storage"
)

type heartbeatJSON map[string]any

// createHeartbeatMonitor creates a heartbeat monitor through the API and
// returns its ID and the heartbeat object of the response.
func createHeartbeatMonitor(t *testing.T, env *testEnv, prefix string) (int64, heartbeatJSON) {
	t.Helper()
	w := env.do("POST", prefix+"/api/v1/monitors", `{"name":"Nightly backup","type":"heartbeat","interval":3600,"settings":{"grace":300}}`, admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Monitor   storage.Monitor `json:"monitor"`
		Heartbeat heartbeatJSON   `json:"heartbeat"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Monitor.ID, resp.Heartbeat
}

func tokenOf(t *testing.T, hb heartbeatJSON) string {
	t.Helper()
	tok, _ := hb["token"].(string)
	if len(tok) != 64 {
		t.Fatalf("heartbeat token = %q, want 64 hex chars", tok)
	}
	if _, ok := hb["url"]; ok {
		t.Error("API returns a url; it is token-only like the agents API")
	}
	return tok
}

func ping(env *testEnv, prefix, method, token string) int {
	return env.do(method, prefix+"/api/v1/heartbeat/"+token, "", nil).Code
}

func TestHeartbeatAPICreateReturnsWorkingToken(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "")
	token := tokenOf(t, hb)
	if hb["grace"] != float64(300) || hb["status"] != "pending" {
		t.Errorf("heartbeat = %v, want grace 300 and status pending", hb)
	}

	for _, method := range []string{"GET", "POST"} {
		if code := ping(env, "", method, token); code != http.StatusOK {
			t.Fatalf("%s ping with the returned token: %d", method, code)
		}
	}
	got, err := env.store.GetHeartbeatByMonitorID(context.Background(), id)
	if err != nil || got.Status != "up" {
		t.Fatalf("after ping: %+v, %v; want status up", got, err)
	}
}

// GET used to return the stored sha256 hash as "token", which looks usable
// but 404s. Neither the hash nor the token may appear.
func TestHeartbeatAPIGetDoesNotExposeToken(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "")
	token := tokenOf(t, hb)
	stored, err := env.store.GetHeartbeatByMonitorID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}

	w := env.do("GET", "/api/v1/monitors/"+strconv.FormatInt(id, 10), "", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, token) || strings.Contains(body, stored.Token) {
		t.Fatalf("GET exposes the token or its hash: %s", body)
	}
	var resp struct {
		Heartbeat heartbeatJSON `json:"heartbeat"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if _, ok := resp.Heartbeat["token"]; ok {
		t.Error("GET heartbeat still has a token field")
	}
	if resp.Heartbeat["grace"] != float64(300) || resp.Heartbeat["status"] != "pending" {
		t.Errorf("GET heartbeat = %v, want grace and status kept", resp.Heartbeat)
	}
}

func TestHeartbeatAPIRegenerate(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "")
	oldToken := tokenOf(t, hb)
	path := "/api/v1/monitors/" + strconv.FormatInt(id, 10) + "/heartbeat/regenerate"

	w := env.do("POST", path, "", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("regenerate: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Heartbeat heartbeatJSON `json:"heartbeat"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	newToken := tokenOf(t, resp.Heartbeat)
	if newToken == oldToken {
		t.Fatal("regenerate returned the same token")
	}
	if code := ping(env, "", "GET", oldToken); code != http.StatusNotFound {
		t.Errorf("old token after regenerate: %d, want 404", code)
	}
	if code := ping(env, "", "GET", newToken); code != http.StatusOK {
		t.Errorf("new token: %d, want 200", code)
	}

	audit, err := env.store.ListAuditLog(context.Background(), storage.AuditLogFilter{Action: "regenerate_token", Entity: "heartbeat"}, storage.Pagination{Page: 1, PerPage: 10})
	if err != nil || audit.Total != 1 {
		t.Errorf("audit = %+v, %v; want one regenerate_token entry", audit, err)
	}

	if w := env.do("POST", "/api/v1/monitors/999999/heartbeat/regenerate", "", admin); w.Code != http.StatusNotFound {
		t.Errorf("unknown monitor: %d, want 404", w.Code)
	}

	w = env.do("POST", "/api/v1/monitors", `{"name":"api","type":"http","target":"https://example.com","interval":60,"timeout":10}`, admin)
	var httpMon storage.Monitor
	json.Unmarshal(w.Body.Bytes(), &httpMon)
	if w := env.do("POST", "/api/v1/monitors/"+strconv.FormatInt(httpMon.ID, 10)+"/heartbeat/regenerate", "", admin); w.Code != http.StatusBadRequest {
		t.Errorf("http monitor: %d, want 400", w.Code)
	}
}

func TestHeartbeatAPIRegenerateNeedsWrite(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) {
		c.Auth.APIKeys = append(c.Auth.APIKeys, config.APIKeyConfig{Name: "viewer", Hash: config.HashAPIKey("viewer-key"), Permissions: []string{"monitors.read"}})
	}, false, nil)
	id, _ := createHeartbeatMonitor(t, env, "")
	w := env.do("POST", "/api/v1/monitors/"+strconv.FormatInt(id, 10)+"/heartbeat/regenerate", "", map[string]string{"X-API-Key": "viewer-key"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("read-only key: %d, want 403", w.Code)
	}
}

func TestHeartbeatAPICloneGetsOwnToken(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "")
	srcToken := tokenOf(t, hb)

	w := env.do("POST", "/api/v1/monitors/"+strconv.FormatInt(id, 10)+"/clone", "", admin)
	if w.Code != http.StatusCreated {
		t.Fatalf("clone: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Monitor   storage.Monitor `json:"monitor"`
		Heartbeat heartbeatJSON   `json:"heartbeat"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	cloneToken := tokenOf(t, resp.Heartbeat)
	if cloneToken == srcToken || resp.Monitor.ID == id {
		t.Fatal("clone shares the source's token or ID")
	}
	got, err := env.store.GetHeartbeatByToken(context.Background(), cloneToken)
	if err != nil || got.MonitorID != resp.Monitor.ID {
		t.Fatalf("clone token resolves to %+v, %v; want monitor %d", got, err, resp.Monitor.ID)
	}
	if got.Grace != 300 {
		t.Errorf("clone grace = %d, want 300", got.Grace)
	}
}

func TestHeartbeatAPIUpdateSyncsRow(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "")
	token := tokenOf(t, hb)
	path := "/api/v1/monitors/" + strconv.FormatInt(id, 10)
	ctx := context.Background()

	if w := env.do("PUT", path, `{"name":"Nightly backup","type":"heartbeat","interval":3600,"settings":{"grace":60}}`, admin); w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	got, err := env.store.GetHeartbeatByToken(ctx, token)
	if err != nil || got.Grace != 60 {
		t.Fatalf("after grace edit: %+v, %v; want token kept and grace 60", got, err)
	}

	if w := env.do("PUT", path, `{"name":"now http","type":"http","target":"https://example.com","interval":60,"timeout":10,"failure_threshold":1,"success_threshold":1}`, admin); w.Code != http.StatusOK {
		t.Fatalf("update to http: %d %s", w.Code, w.Body.String())
	}
	if code := ping(env, "", "GET", token); code != http.StatusNotFound {
		t.Errorf("ping after type change: %d, want 404", code)
	}

	// Back to heartbeat: no token is minted silently; regenerate creates one.
	if w := env.do("PUT", path, `{"name":"Nightly backup","type":"heartbeat","interval":3600}`, admin); w.Code != http.StatusOK {
		t.Fatalf("update back to heartbeat: %d %s", w.Code, w.Body.String())
	}
	if _, err := env.store.GetHeartbeatByMonitorID(ctx, id); err == nil {
		t.Error("heartbeat row created by a type change, with a token nobody saw")
	}
	w := env.do("POST", path+"/heartbeat/regenerate", "", admin)
	var resp struct {
		Heartbeat heartbeatJSON `json:"heartbeat"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if code := ping(env, "", "POST", tokenOf(t, resp.Heartbeat)); code != http.StatusOK {
		t.Errorf("first token after type change: %d, want 200", code)
	}
}

func TestHeartbeatAPIUnderBasePath(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.Server.BasePath = "/asura" }, false, nil)
	id, hb := createHeartbeatMonitor(t, env, "/asura")
	if code := ping(env, "/asura", "GET", tokenOf(t, hb)); code != http.StatusOK {
		t.Fatalf("ping under base path: %d", code)
	}
	w := env.do("POST", "/asura/api/v1/monitors/"+strconv.FormatInt(id, 10)+"/heartbeat/regenerate", "", admin)
	if w.Code != http.StatusOK {
		t.Fatalf("regenerate under base path: %d %s", w.Code, w.Body.String())
	}
}

// A clone whose heartbeat cannot be created must not look like a success
// without a token: it reports the failure and how to get a token.
func TestHeartbeatAPICloneReportsHeartbeatFailure(t *testing.T) {
	env := newTestEnv(t, nil, false, nil)
	id, _ := createHeartbeatMonitor(t, env, "")

	db, err := sql.Open("sqlite", "file:"+env.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER fail_heartbeat_insert BEFORE INSERT ON heartbeats BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}

	w := env.do("POST", "/api/v1/monitors/"+strconv.FormatInt(id, 10)+"/clone", "", admin)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("clone with failing heartbeat: %d %s, want 500", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "/heartbeat/regenerate") {
		t.Errorf("error does not say how to get a token: %s", w.Body.String())
	}
}
