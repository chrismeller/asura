package web

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/y0f/asura/internal/httputil"
)

// secretReveal holds a just-created secret (an agent or heartbeat token)
// between the POST that created it and the redirected GET that shows it. The
// store keeps only hashes, so this is the one time the plaintext is
// available; refreshing the page does not resubmit the form.
type secretReveal struct {
	kind      string // "agent" or "heartbeat"
	ref       int64  // ID of the agent or monitor the secret belongs to
	name      string
	token     string
	owner     string
	createdAt time.Time
}

const revealTTL = 10 * time.Minute

// storeReveal keeps a secret for one later takeReveal and returns the opaque
// id to pass in the redirect URL, or "" if no id could be generated.
func (h *Handler) storeReveal(r *http.Request, kind string, ref int64, name, token string) string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	id := hex.EncodeToString(b)
	h.revealMu.Lock()
	defer h.revealMu.Unlock()
	if h.reveals == nil {
		h.reveals = make(map[string]secretReveal)
	}
	for k, v := range h.reveals {
		if time.Since(v.createdAt) > revealTTL {
			delete(h.reveals, k)
		}
	}
	h.reveals[id] = secretReveal{kind: kind, ref: ref, name: name, token: token, owner: httputil.GetAPIKeyName(r.Context()), createdAt: time.Now()}
	return id
}

// takeReveal returns and forgets a pending reveal of the given kind. It only
// matches the API key that created the secret, and only within revealTTL.
// Any lookup of an id consumes it, so a reveal is shown at most once.
func (h *Handler) takeReveal(r *http.Request, id, kind string) (secretReveal, bool) {
	if id == "" {
		return secretReveal{}, false
	}
	h.revealMu.Lock()
	defer h.revealMu.Unlock()
	v, ok := h.reveals[id]
	if !ok {
		return secretReveal{}, false
	}
	delete(h.reveals, id)
	if v.kind != kind || time.Since(v.createdAt) > revealTTL || v.owner != httputil.GetAPIKeyName(r.Context()) {
		return secretReveal{}, false
	}
	return v, true
}
