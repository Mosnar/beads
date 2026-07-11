//go:build cgo

package main

import (
	"strings"
	"testing"

	"github.com/steveyegge/beads/internal/types"
)

// TestUnclaimIfAssigneeCLI drives `bd unclaim --if-assignee` end-to-end against
// the SQLite backend (always available; no env gate): the conditional release
// must be a compare-and-swap, not a read-then-clobber. A stale expectation exits
// nonzero, names the current holder, and leaves the claim untouched; the
// matching expectation releases the claim.
func TestUnclaimIfAssigneeCLI(t *testing.T) {
	t.Parallel()

	bd := buildEmbeddedBD(t)
	dir, _, _ := bdInit(t, bd, "--prefix", "ur", "--backend", "sqlite")

	issue := bdCreate(t, bd, dir, "Conditional release", "--type", "task")
	bdUpdate(t, bd, dir, issue.ID, "--assignee", "alice", "--status", "in_progress")

	// Stale expectation: distinct failure that names the current holder, claim intact.
	out := bdUnclaimFail(t, bd, dir, issue.ID, "--if-assignee", "bob")
	if !strings.Contains(out, "alice") {
		t.Errorf("mismatch error should name the current holder alice, got:\n%s", out)
	}
	got := bdShow(t, bd, dir, issue.ID)
	if got.Assignee != "alice" {
		t.Errorf("stale --if-assignee clobbered the claim: assignee = %q, want alice", got.Assignee)
	}
	if got.Status != types.StatusInProgress {
		t.Errorf("stale --if-assignee changed status to %q, want in_progress", got.Status)
	}

	// Matching expectation: releases the claim.
	bdUnclaim(t, bd, dir, issue.ID, "--if-assignee", "alice")
	got = bdShow(t, bd, dir, issue.ID)
	if got.Assignee != "" {
		t.Errorf("after matching --if-assignee: assignee = %q, want empty", got.Assignee)
	}
	if got.Status != types.StatusOpen {
		t.Errorf("after matching --if-assignee: status = %q, want open", got.Status)
	}

	// Releasing an already-released issue with --if-assignee is also a distinct
	// failure (exactly-once), not a silent success.
	_ = bdUnclaimFail(t, bd, dir, issue.ID, "--if-assignee", "alice")
}
