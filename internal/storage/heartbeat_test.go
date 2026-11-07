package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
)

func TestSetHeartbeatTokenCreatesMissingRow(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	m := &Monitor{Name: "UI heartbeat", Type: "heartbeat", Target: "heartbeat", Interval: 300, Timeout: 10, Enabled: true, Tags: []string{}, FailureThreshold: 1, SuccessThreshold: 1}
	if err := store.CreateMonitor(ctx, m); err != nil {
		t.Fatal(err)
	}

	if err := store.SetHeartbeatToken(ctx, m.ID, "first-token", 30); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetHeartbeatByToken(ctx, "first-token")
	if err != nil {
		t.Fatalf("token not found after create: %v", err)
	}
	if got.MonitorID != m.ID || got.Grace != 30 || got.Status != "pending" {
		t.Fatalf("unexpected row: %+v", got)
	}
}

func TestSetHeartbeatTokenReplacesTokenAndKeepsState(t *testing.T) {
	store, ctx, m := createTestHeartbeat(t)
	if err := store.UpdateHeartbeatPing(ctx, "abc123def456"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateHeartbeatStatus(ctx, m.ID, "down"); err != nil {
		t.Fatal(err)
	}

	if err := store.SetHeartbeatToken(ctx, m.ID, "rotated-token", 90); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetHeartbeatByToken(ctx, "abc123def456"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("old token still resolves: err=%v", err)
	}
	got, err := store.GetHeartbeatByToken(ctx, "rotated-token")
	if err != nil {
		t.Fatalf("new token not found: %v", err)
	}
	// A down heartbeat must stay down so the next ping runs recovery and
	// resolves the open incident.
	if got.Status != "down" {
		t.Fatalf("status = %q, want down kept", got.Status)
	}
	if got.LastPingAt == nil {
		t.Fatal("last_ping_at was cleared")
	}
	if got.Grace != 90 {
		t.Fatalf("grace = %d, want 90", got.Grace)
	}
}

func TestUpdateHeartbeatGrace(t *testing.T) {
	store, ctx, m := createTestHeartbeat(t)
	if err := store.UpdateHeartbeatGrace(ctx, m.ID, 120); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetHeartbeatByMonitorID(ctx, m.ID)
	if got.Grace != 120 {
		t.Fatalf("grace = %d, want 120", got.Grace)
	}
	// No row: nothing to update, and no error.
	if err := store.UpdateHeartbeatGrace(ctx, m.ID+1000, 5); err != nil {
		t.Fatal(err)
	}
}

func TestSyncHeartbeatOnUpdate(t *testing.T) {
	store, ctx, m := createTestHeartbeat(t)

	m.Settings = json.RawMessage(`{"grace":45}`)
	if err := SyncHeartbeatOnUpdate(ctx, store, "heartbeat", m); err != nil {
		t.Fatal(err)
	}
	got, _ := store.GetHeartbeatByMonitorID(ctx, m.ID)
	if got.Grace != 45 {
		t.Fatalf("grace = %d, want 45", got.Grace)
	}

	m.Type = "http"
	if err := SyncHeartbeatOnUpdate(ctx, store, "heartbeat", m); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetHeartbeatByToken(ctx, "abc123def456"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("heartbeat row kept after type change: err=%v", err)
	}

	// Becoming a heartbeat does not mint a token nobody would see.
	m.Type = "heartbeat"
	if err := SyncHeartbeatOnUpdate(ctx, store, "http", m); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetHeartbeatByMonitorID(ctx, m.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row created on type change: err=%v", err)
	}
}

func TestHeartbeatGrace(t *testing.T) {
	cases := map[string]int{
		``:               0,
		`null`:           0,
		`{}`:             0,
		`{"grace":300}`:  300,
		`{"grace":-5}`:   0,
		`{"grace":"30"}`: 0,
		`not json`:       0,
	}
	for in, want := range cases {
		if got := HeartbeatGrace(json.RawMessage(in)); got != want {
			t.Errorf("HeartbeatGrace(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestNewHeartbeatToken(t *testing.T) {
	a, err := NewHeartbeatToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewHeartbeatToken()
	if len(a) != 64 || a == b {
		t.Fatalf("tokens %q, %q: want two distinct 64-char hex strings", a, b)
	}
}
