package web

import (
	"fmt"
	"net/http"

	"github.com/y0f/asura/internal/httputil"
	"github.com/y0f/asura/internal/storage"
	"github.com/y0f/asura/internal/web/views"
)

func (h *Handler) storeAgentReveal(r *http.Request, a *storage.Agent) string {
	return h.storeReveal(r, "agent", a.ID, a.Name, a.Token)
}

// takeAgentReveal returns and forgets a pending agent token reveal.
func (h *Handler) takeAgentReveal(r *http.Request, id string) *storage.Agent {
	v, ok := h.takeReveal(r, id, "agent")
	if !ok {
		return nil
	}
	return &storage.Agent{Name: v.name, Token: v.token}
}

func (h *Handler) Agents(w http.ResponseWriter, r *http.Request) {
	h.renderAgents(w, r, h.takeAgentReveal(r, r.URL.Query().Get("created")))
}

// renderAgents renders the agent list. created, when set, is an agent that was
// just created: its token is shown once in a panel instead of a toast.
func (h *Handler) renderAgents(w http.ResponseWriter, r *http.Request, created *storage.Agent) {
	agents, err := h.store.ListAgents(r.Context())
	if err != nil {
		h.logger.Error("web: list agents", "error", err)
	}
	lp := h.newLayoutParams(r, "Agents", "agents")
	params := views.AgentListParams{
		LayoutParams: lp,
		Agents:       agents,
		ServerURL:    h.cfg.ResolvedExternalURL(),
	}
	if created != nil {
		params.NewAgentName = created.Name
		params.NewAgentToken = created.Token
		w.Header().Set("Cache-Control", "no-store")
	}
	h.renderComponent(w, r, views.AgentListPage(params))
}

func (h *Handler) AgentCreate(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	location := r.FormValue("location")
	if name == "" {
		h.setError(w, "Name is required")
		h.redirect(w, r, "/agents")
		return
	}
	a := &storage.Agent{Name: name, Location: location, Enabled: true}
	if err := h.store.CreateAgent(r.Context(), a); err != nil {
		h.logger.Error("web: create agent", "error", err)
		h.setError(w, "Failed to create agent")
		h.redirect(w, r, "/agents")
		return
	}
	h.audit(r, "create", "agent", a.ID, a.Name)
	if id := h.storeAgentReveal(r, a); id != "" {
		h.redirect(w, r, "/agents?created="+id)
		return
	}
	h.setError(w, "Agent created, but its token could not be shown. Delete it and create it again.")
	h.redirect(w, r, "/agents")
}

func (h *Handler) AgentDelete(w http.ResponseWriter, r *http.Request) {
	id, err := httputil.ParseID(r)
	if err != nil {
		h.redirect(w, r, "/agents")
		return
	}
	disabled, err := h.store.DeleteAgent(r.Context(), id)
	if err != nil {
		h.logger.Error("web: delete agent", "error", err)
		h.setError(w, "Failed to delete agent")
		h.redirect(w, r, "/agents")
		return
	}
	h.audit(r, "delete", "agent", id, "")
	for _, monID := range disabled {
		h.logger.Warn("monitor disabled for agents: its only assigned agent was deleted", "monitor_id", monID, "agent_id", id)
		h.audit(r, "update", "monitor", monID, fmt.Sprintf("agent checks disabled: only assigned agent %d was deleted", id))
	}
	if len(disabled) > 0 {
		h.setFlash(w, fmt.Sprintf("Agent deleted. %d monitor(s) assigned only to it no longer run on agents.", len(disabled)))
	} else {
		h.setFlash(w, "Agent deleted")
	}
	h.redirect(w, r, "/agents")
}
