package main

import (
	"path/filepath"
	"testing"
	"time"
)

func TestResolveStatePathRuntime(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	got, err := resolveStatePath("/tmp/default-state.json", true, false)
	if err != nil {
		t.Fatalf("resolveStatePath returned error: %v", err)
	}

	want := filepath.Join("/run/user/1000", "mail-notify", "state.json")
	if got != want {
		t.Fatalf("state path = %q, want %q", got, want)
	}
}

func TestResolveStatePathExplicitWins(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")

	got, err := resolveStatePath("/tmp/custom-state.json", true, true)
	if err != nil {
		t.Fatalf("resolveStatePath returned error: %v", err)
	}
	if got != "/tmp/custom-state.json" {
		t.Fatalf("state path = %q, want explicit path", got)
	}
}

func TestResolveStatePathRuntimeRequiresEnv(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	if _, err := resolveStatePath("/tmp/default-state.json", true, false); err == nil {
		t.Fatal("expected error when XDG_RUNTIME_DIR is unset")
	}
}

func TestTouchAccountStateUpdatesExistingOnly(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	state := stateFile{Accounts: map[string]mailboxState{
		"present": {UIDNext: 10, Unseen: 2},
	}}

	if !touchAccountState(state, "present", now) {
		t.Fatal("expected existing account state to change")
	}
	if got := state.Accounts["present"].LastSeen; !got.Equal(now) {
		t.Fatalf("last_seen = %s, want %s", got, now)
	}
	if touchAccountState(state, "missing", now) {
		t.Fatal("did not expect missing account state to be created")
	}
	if _, ok := state.Accounts["missing"]; ok {
		t.Fatal("missing account state was created")
	}
}

func TestPruneStaleAccountStates(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	state := stateFile{Accounts: map[string]mailboxState{
		"seen":           {LastSeen: now.Add(-30 * 24 * time.Hour)},
		"recent-missing": {LastSeen: now.Add(-9 * 24 * time.Hour)},
		"stale-missing":  {LastSeen: now.Add(-11 * 24 * time.Hour)},
		"legacy-missing": {},
	}}
	seen := map[string]struct{}{"seen": {}}

	if !pruneStaleAccountStates(state, seen, now, staleStateAfter, nil) {
		t.Fatal("expected prune to change state")
	}

	if _, ok := state.Accounts["seen"]; !ok {
		t.Fatal("seen account was pruned")
	}
	if _, ok := state.Accounts["recent-missing"]; !ok {
		t.Fatal("recent missing account was pruned")
	}
	if _, ok := state.Accounts["stale-missing"]; ok {
		t.Fatal("stale missing account was not pruned")
	}
	if got := state.Accounts["legacy-missing"].LastSeen; !got.Equal(now) {
		t.Fatalf("legacy missing last_seen = %s, want %s", got, now)
	}
}
