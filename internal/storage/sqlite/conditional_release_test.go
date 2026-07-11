package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// TestUnclaimIfAssigneeTwoHandles exercises the conditional-release CAS across
// two independent store handles on the same SQLite file — the multiprocess shape
// a release-if-current caller (e.g. gc returning a dead worker's bead) actually
// runs in. The conditional UPDATE ... WHERE assignee = ? plus RowsAffected is
// the same idiom ClaimIssueInTx uses on the claim side; this pins the release
// side: a matching expected assignee releases exactly once, and a stale expected
// assignee is a loud no-op (storage.ErrAssigneeMismatch, claim untouched).
func TestUnclaimIfAssigneeTwoHandles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cas.db")

	h1, err := Provision(ctx, path)
	if err != nil {
		t.Fatalf("Provision handle 1: %v", err)
	}
	t.Cleanup(func() { _ = h1.Close() })
	if err := h1.SetConfig(ctx, "issue_prefix", "test"); err != nil {
		t.Fatalf("SetConfig(issue_prefix): %v", err)
	}

	h2, err := Provision(ctx, path)
	if err != nil {
		t.Fatalf("Provision handle 2: %v", err)
	}
	t.Cleanup(func() { _ = h2.Close() })

	issue := withDefaults(&types.Issue{ID: "test-cas-1", Title: "conditional release"})
	if err := h1.CreateIssue(ctx, issue, "creator"); err != nil {
		t.Fatalf("CreateIssue: %v", err)
	}
	if err := h1.ClaimIssue(ctx, "test-cas-1", "owner"); err != nil {
		t.Fatalf("ClaimIssue: %v", err)
	}

	// A releaser with a stale view (expects a different owner) must not clobber
	// the live claim, and must hear about it loudly.
	err = h2.UnclaimIssueIfAssignee(ctx, "test-cas-1", "gc", "someone-else")
	if !errors.Is(err, storage.ErrAssigneeMismatch) {
		t.Fatalf("stale release via handle 2: err = %v, want ErrAssigneeMismatch", err)
	}
	got, err := h1.GetIssue(ctx, "test-cas-1")
	if err != nil {
		t.Fatalf("GetIssue after stale release: %v", err)
	}
	if got.Assignee != "owner" || got.Status != types.StatusInProgress {
		t.Fatalf("stale release disturbed the claim: assignee=%q status=%q", got.Assignee, got.Status)
	}

	// The matching release wins — exactly once, from either handle.
	if err := h2.UnclaimIssueIfAssignee(ctx, "test-cas-1", "gc", "owner"); err != nil {
		t.Fatalf("matching release via handle 2: %v", err)
	}
	// A second release with the same expectation (handle 1's stale view of the
	// claim) loses the CAS: the claim is already gone.
	err = h1.UnclaimIssueIfAssignee(ctx, "test-cas-1", "gc", "owner")
	if !errors.Is(err, storage.ErrAssigneeMismatch) {
		t.Fatalf("repeat release via handle 1: err = %v, want ErrAssigneeMismatch", err)
	}

	got, err = h2.GetIssue(ctx, "test-cas-1")
	if err != nil {
		t.Fatalf("GetIssue after release: %v", err)
	}
	if got.Assignee != "" || got.Status != types.StatusOpen {
		t.Fatalf("after release: assignee=%q status=%q, want unassigned/open", got.Assignee, got.Status)
	}
}
