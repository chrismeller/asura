package web

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/y0f/asura/internal/config"
	"github.com/y0f/asura/internal/httputil"
	"github.com/y0f/asura/internal/storage"
)

var pingURLRe = regexp.MustCompile(`(https?://[^\s"<]+)/api/v1/heartbeat/([0-9a-f]+)`)

func heartbeatForm(extra url.Values) url.Values {
	f := url.Values{
		"name": {"Nightly backup"}, "type": {"heartbeat"},
		"interval": {"3600"}, "timeout": {"10"}, "failure_threshold": {"1"}, "success_threshold": {"1"},
		"settings_mode": {"form"}, "assertions_mode": {"form"},
		"settings_heartbeat_grace": {"300"},
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

// requestAs is adminRequest for another API key.
func requestAs(keyName, method, target string, form url.Values) *http.Request {
	r := adminRequest(method, target, form)
	key := &config.APIKeyConfig{Name: keyName, Role: "admin", Permissions: []string{"monitors.read", "monitors.write"}}
	return r.WithContext(context.WithValue(r.Context(), httputil.CtxKeyAPIKey, key))
}

func onlyMonitor(t *testing.T, store *storage.SQLiteStore, notID int64) *storage.Monitor {
	t.Helper()
	res, err := store.ListMonitors(context.Background(), storage.MonitorListFilter{}, storage.Pagination{Page: 1, PerPage: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range res.Data.([]*storage.Monitor) {
		if m.ID != notID {
			return m
		}
	}
	t.Fatal("monitor not found")
	return nil
}

// followReveal follows a create/clone/regenerate redirect to the monitor page.
func followReveal(t *testing.T, h *Handler, w *httptest.ResponseRecorder, monID int64, keyName string) *httptest.ResponseRecorder {
	t.Helper()
	if w.Code != http.StatusSeeOther {
		t.Fatalf("got %d, want 303: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	want := h.cfg.Server.BasePath + "/monitors/" + strconvI(monID) + "?reveal="
	if !strings.HasPrefix(loc, want) {
		t.Fatalf("redirect = %q, want prefix %q", loc, want)
	}
	return getDetail(h, monID, strings.TrimPrefix(loc, h.cfg.Server.BasePath), keyName)
}

func getDetail(h *Handler, monID int64, target, keyName string) *httptest.ResponseRecorder {
	r := requestAs(keyName, "GET", target, nil)
	r.SetPathValue("id", strconvI(monID))
	w := httptest.NewRecorder()
	h.MonitorDetail(w, r)
	return w
}

func shownToken(t *testing.T, w *httptest.ResponseRecorder, wantBase string) string {
	t.Helper()
	m := pingURLRe.FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatal("monitor page shows no ping URL")
	}
	if m[1] != wantBase {
		t.Fatalf("ping URL base = %q, want %q", m[1], wantBase)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on a page showing a token, want no-store", got)
	}
	return m[2]
}

func assertTokenWorks(t *testing.T, store *storage.SQLiteStore, token string, monID int64) {
	t.Helper()
	hb, err := store.GetHeartbeatByToken(context.Background(), token)
	if err != nil {
		t.Fatalf("shown token does not resolve: %v", err)
	}
	if hb.MonitorID != monID {
		t.Fatalf("token belongs to monitor %d, want %d", hb.MonitorID, monID)
	}
}

func assertNoPingURL(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if strings.Contains(w.Body.String(), "/api/v1/heartbeat/") {
		t.Fatal("ping URL shown again")
	}
}

func TestHeartbeatCreateShowsPingURLOnce(t *testing.T) {
	h, store := storeHandler(t)
	h.cfg.Server.ExternalURL = "https://mon.example.com/asura"

	// settings_mode left on "json" by a previously selected type must not
	// replace the heartbeat settings.
	w := httptest.NewRecorder()
	h.MonitorCreate(w, adminRequest("POST", "/monitors", heartbeatForm(url.Values{
		"settings_mode": {"json"}, "settings_json": {`{"steps":[]}`},
	})))
	mon := onlyMonitor(t, store, 0)

	revealURL := w.Header().Get("Location")
	page := followReveal(t, h, w, mon.ID, "admin")
	token := shownToken(t, page, "https://mon.example.com/asura")
	assertTokenWorks(t, store, token, mon.ID)

	hb, err := store.GetHeartbeatByMonitorID(context.Background(), mon.ID)
	if err != nil {
		t.Fatal(err)
	}
	if hb.Grace != 300 || storage.HeartbeatGrace(mon.Settings) != 300 {
		t.Errorf("grace: row %d, settings %s; want 300", hb.Grace, mon.Settings)
	}

	again := getDetail(h, mon.ID, revealURL, "admin")
	assertNoPingURL(t, again)
	if again.Header().Get("Cache-Control") == "no-store" {
		t.Error("page without a token is marked no-store")
	}
}

func TestHeartbeatRevealUsesBasePath(t *testing.T) {
	h, store := storeHandler(t)
	h.cfg.Server.BasePath = "/asura"
	h.cfg.Server.Listen = "127.0.0.1:8090"

	w := httptest.NewRecorder()
	h.MonitorCreate(w, adminRequest("POST", "/monitors", heartbeatForm(nil)))
	mon := onlyMonitor(t, store, 0)
	token := shownToken(t, followReveal(t, h, w, mon.ID, "admin"), "http://127.0.0.1:8090/asura")
	assertTokenWorks(t, store, token, mon.ID)
}

func TestHeartbeatRevealOnlyForCreatorAndMonitor(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()

	w := httptest.NewRecorder()
	h.MonitorCreate(w, adminRequest("POST", "/monitors", heartbeatForm(nil)))
	mon := onlyMonitor(t, store, 0)
	assertNoPingURL(t, followReveal(t, h, w, mon.ID, "someone-else"))

	other := &storage.Monitor{Name: "Other", Type: "heartbeat", Target: "heartbeat", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, other); err != nil {
		t.Fatal(err)
	}
	r := adminRequest("POST", "/monitors/"+strconvI(mon.ID)+"/heartbeat/regenerate", nil)
	r.SetPathValue("id", strconvI(mon.ID))
	w = httptest.NewRecorder()
	h.MonitorRegenerateHeartbeat(w, r)
	revealQuery := w.Header().Get("Location")[strings.Index(w.Header().Get("Location"), "?"):]
	assertNoPingURL(t, getDetail(h, other.ID, "/monitors/"+strconvI(other.ID)+revealQuery, "admin"))
}

func TestHeartbeatRegenerate(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()
	mon := &storage.Monitor{Name: "Cron", Type: "heartbeat", Target: "heartbeat", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateHeartbeat(ctx, &storage.Heartbeat{MonitorID: mon.ID, Token: "old-token", Status: "up"}); err != nil {
		t.Fatal(err)
	}

	page := getDetail(h, mon.ID, "/monitors/"+strconvI(mon.ID), "admin")
	if !strings.Contains(page.Body.String(), "Regenerate URL") {
		t.Fatal("no Regenerate URL action on a heartbeat monitor page")
	}
	assertNoPingURL(t, page)

	readOnly := httptest.NewRequest("GET", "/monitors/"+strconvI(mon.ID), nil)
	readOnly = readOnly.WithContext(context.WithValue(readOnly.Context(), httputil.CtxKeyAPIKey, &config.APIKeyConfig{Name: "viewer", Permissions: []string{"monitors.read"}}))
	readOnly.SetPathValue("id", strconvI(mon.ID))
	rw := httptest.NewRecorder()
	h.MonitorDetail(rw, readOnly)
	if strings.Contains(rw.Body.String(), "Regenerate URL") {
		t.Error("read-only key is offered Regenerate URL")
	}

	r := adminRequest("POST", "/monitors/"+strconvI(mon.ID)+"/heartbeat/regenerate", nil)
	r.SetPathValue("id", strconvI(mon.ID))
	w := httptest.NewRecorder()
	h.MonitorRegenerateHeartbeat(w, r)

	token := shownToken(t, followReveal(t, h, w, mon.ID, "admin"), h.cfg.ResolvedExternalURL())
	assertTokenWorks(t, store, token, mon.ID)
	if _, err := store.GetHeartbeatByToken(ctx, "old-token"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("old token still works after regenerate: err=%v", err)
	}
	audit, err := store.ListAuditLog(ctx, storage.AuditLogFilter{Action: "regenerate_token", Entity: "heartbeat"}, storage.Pagination{Page: 1, PerPage: 10})
	if err != nil || audit.Total != 1 {
		t.Errorf("audit entries = %+v, %v; want 1", audit, err)
	}
}

func TestHeartbeatRegenerateRejectsOtherTypes(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()
	mon := &storage.Monitor{Name: "API", Type: "http", Target: "https://example.com", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	r := adminRequest("POST", "/monitors/"+strconvI(mon.ID)+"/heartbeat/regenerate", nil)
	r.SetPathValue("id", strconvI(mon.ID))
	w := httptest.NewRecorder()
	h.MonitorRegenerateHeartbeat(w, r)
	if !errorToast(w) {
		t.Error("no error shown")
	}
	if _, err := store.GetHeartbeatByMonitorID(ctx, mon.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("heartbeat row created for an http monitor: err=%v", err)
	}
}

// A heartbeat monitor without a row (made by older versions of the web form,
// an import, or a type change) offers to generate its first URL.
func TestHeartbeatWithoutTokenOffersGenerate(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()
	mon := &storage.Monitor{Name: "Legacy", Type: "heartbeat", Target: "heartbeat", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	page := getDetail(h, mon.ID, "/monitors/"+strconvI(mon.ID), "admin")
	if !strings.Contains(page.Body.String(), "Generate URL") || !strings.Contains(page.Body.String(), "no ping URL yet") {
		t.Fatal("monitor without a token does not offer Generate URL")
	}

	r := adminRequest("POST", "/monitors/"+strconvI(mon.ID)+"/heartbeat/regenerate", nil)
	r.SetPathValue("id", strconvI(mon.ID))
	w := httptest.NewRecorder()
	h.MonitorRegenerateHeartbeat(w, r)
	token := shownToken(t, followReveal(t, h, w, mon.ID, "admin"), h.cfg.ResolvedExternalURL())
	assertTokenWorks(t, store, token, mon.ID)
}

func TestHeartbeatCloneGetsOwnToken(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()
	src := &storage.Monitor{Name: "Cron", Type: "heartbeat", Target: "heartbeat", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, src); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateHeartbeat(ctx, &storage.Heartbeat{MonitorID: src.ID, Token: "source-token", Status: "up"}); err != nil {
		t.Fatal(err)
	}

	r := adminRequest("POST", "/monitors/"+strconvI(src.ID)+"/clone", nil)
	r.SetPathValue("id", strconvI(src.ID))
	w := httptest.NewRecorder()
	h.MonitorClone(w, r)
	clone := onlyMonitor(t, store, src.ID)

	token := shownToken(t, followReveal(t, h, w, clone.ID, "admin"), h.cfg.ResolvedExternalURL())
	assertTokenWorks(t, store, token, clone.ID)
	assertTokenWorks(t, store, "source-token", src.ID)
}

func TestHeartbeatEditSyncsRow(t *testing.T) {
	h, store := storeHandler(t)
	ctx := context.Background()
	mon := &storage.Monitor{Name: "Cron", Type: "heartbeat", Target: "heartbeat", Interval: 60, Timeout: 10, Enabled: true, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, mon); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateHeartbeat(ctx, &storage.Heartbeat{MonitorID: mon.ID, Token: "tok", Status: "up"}); err != nil {
		t.Fatal(err)
	}

	update := func(form url.Values) {
		t.Helper()
		r := adminRequest("POST", "/monitors/"+strconvI(mon.ID), form)
		r.SetPathValue("id", strconvI(mon.ID))
		w := httptest.NewRecorder()
		h.MonitorUpdate(w, r)
		if w.Code != http.StatusSeeOther || errorToast(w) {
			t.Fatalf("update returned %d: %s", w.Code, w.Body.String())
		}
	}

	update(heartbeatForm(url.Values{"settings_heartbeat_grace": {"120"}}))
	hb, err := store.GetHeartbeatByToken(ctx, "tok")
	if err != nil {
		t.Fatalf("token stopped working after an edit: %v", err)
	}
	if hb.Grace != 120 {
		t.Errorf("grace = %d after edit, want 120", hb.Grace)
	}

	update(httpMonitorForm("", nil))
	if _, err := store.GetHeartbeatByToken(ctx, "tok"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("ping URL still works after the monitor stopped being a heartbeat: err=%v", err)
	}
}
