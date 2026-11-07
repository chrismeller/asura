package storage

import (
	"context"
	"encoding/json"
)

// HeartbeatSettings is the settings payload of a heartbeat monitor.
type HeartbeatSettings struct {
	// Grace is how many seconds past the monitor interval a ping may be late
	// before the heartbeat counts as missed.
	Grace int `json:"grace,omitempty"`
}

// NewHeartbeatToken returns a random heartbeat token. Only its hash is
// stored, so the plaintext must be shown to the user when it is created.
func NewHeartbeatToken() (string, error) {
	return generateToken()
}

// HeartbeatGrace reads the grace period from a heartbeat monitor's settings.
// Missing, malformed or negative values count as no grace.
func HeartbeatGrace(settings json.RawMessage) int {
	if len(settings) == 0 {
		return 0
	}
	var s struct {
		Grace float64 `json:"grace"`
	}
	if err := json.Unmarshal(settings, &s); err != nil || s.Grace < 0 {
		return 0
	}
	return int(s.Grace)
}

// SyncHeartbeatOnUpdate keeps a monitor's heartbeat row in step with an edit:
// a heartbeat monitor picks up its new grace period, and a monitor that is no
// longer a heartbeat loses its row, so its old ping URL stops working. A
// monitor that just became a heartbeat gets no row here; its token is created
// by an explicit regenerate, the only place it can be shown to the user.
func SyncHeartbeatOnUpdate(ctx context.Context, s Store, oldType string, m *Monitor) error {
	switch {
	case m.Type == "heartbeat":
		return s.UpdateHeartbeatGrace(ctx, m.ID, HeartbeatGrace(m.Settings))
	case oldType == "heartbeat":
		return s.DeleteHeartbeat(ctx, m.ID)
	}
	return nil
}
