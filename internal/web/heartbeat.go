package web

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"

	"github.com/y0f/asura/internal/httputil"
	"github.com/y0f/asura/internal/storage"
	"github.com/y0f/asura/internal/web/views"
)

// issueHeartbeatToken gives mon a new heartbeat token, creating its heartbeat
// row if needed, and returns the plaintext. Any previous token stops working.
func (h *Handler) issueHeartbeatToken(r *http.Request, mon *storage.Monitor) (string, error) {
	token, err := storage.NewHeartbeatToken()
	if err != nil {
		return "", err
	}
	if err := h.store.SetHeartbeatToken(r.Context(), mon.ID, token, storage.HeartbeatGrace(mon.Settings)); err != nil {
		return "", err
	}
	return token, nil
}

// redirectWithHeartbeatToken issues mon's first heartbeat token after a
// create or clone and redirects to the monitor page, which shows it once.
// assignErr is a non-fatal agent-assignment error to show instead of flash.
func (h *Handler) redirectWithHeartbeatToken(w http.ResponseWriter, r *http.Request, mon *storage.Monitor, flash, assignErr string) {
	dest := "/monitors/" + strconv.FormatInt(mon.ID, 10)
	token, err := h.issueHeartbeatToken(r, mon)
	if err != nil {
		h.logger.Error("web: create heartbeat", "error", err)
		h.setError(w, flash+", but its ping URL could not be created. Use Generate URL on this page to try again.")
		h.redirect(w, r, dest)
		return
	}
	revealID := h.storeReveal(r, "heartbeat", mon.ID, mon.Name, token)
	switch {
	case assignErr != "":
		h.setError(w, assignErr)
	case revealID == "":
		h.setError(w, flash+", but its ping URL could not be shown. Use Regenerate URL to get a new one.")
	default:
		h.setFlash(w, flash)
	}
	if revealID != "" {
		dest += "?reveal=" + revealID
	}
	h.redirect(w, r, dest)
}

// MonitorRegenerateHeartbeat replaces a heartbeat monitor's token (or creates
// the first one) and shows the new ping URL once. The old URL stops working.
func (h *Handler) MonitorRegenerateHeartbeat(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.ParseID(r)
	if err != nil {
		h.redirect(w, r, "/monitors")
		return
	}
	mon, err := h.store.GetMonitor(r.Context(), id)
	if err != nil {
		h.setError(w, "Monitor not found")
		h.redirect(w, r, "/monitors")
		return
	}
	dest := "/monitors/" + strconv.FormatInt(id, 10)
	if mon.Type != "heartbeat" {
		h.setError(w, "Only heartbeat monitors have a ping URL")
		h.redirect(w, r, dest)
		return
	}
	token, err := h.issueHeartbeatToken(r, mon)
	if err != nil {
		h.logger.Error("web: regenerate heartbeat token", "error", err)
		h.setError(w, "Failed to generate a new ping URL")
		h.redirect(w, r, dest)
		return
	}
	h.audit(r, "regenerate_token", "heartbeat", id, "")
	revealID := h.storeReveal(r, "heartbeat", id, mon.Name, token)
	if revealID == "" {
		h.setError(w, "A new ping URL was created but could not be shown. Regenerate it again.")
		h.redirect(w, r, dest)
		return
	}
	h.setFlash(w, "New ping URL generated. The previous URL no longer works.")
	h.redirect(w, r, dest+"?reveal="+revealID)
}

// heartbeatView builds the ping URL panel for a heartbeat monitor's page. The
// URL is only included right after its token was created (?reveal=), since
// the store keeps just the token's hash.
func (h *Handler) heartbeatView(r *http.Request, mon *storage.Monitor) *views.HeartbeatView {
	v := &views.HeartbeatView{ServerURL: h.cfg.ResolvedExternalURL()}
	hb, err := h.store.GetHeartbeatByMonitorID(r.Context(), mon.ID)
	switch {
	case err == nil:
		v.Exists = true
		v.Status = hb.Status
		v.Grace = hb.Grace
		v.LastPingAt = hb.LastPingAt
	case !errors.Is(err, sql.ErrNoRows):
		h.logger.Error("web: get heartbeat", "error", err)
	}
	if rv, ok := h.takeReveal(r, r.URL.Query().Get("reveal"), "heartbeat"); ok && rv.ref == mon.ID {
		v.NewToken = rv.token
	}
	return v
}
