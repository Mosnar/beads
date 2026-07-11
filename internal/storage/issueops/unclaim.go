package issueops

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/types"
)

// UnclaimIssueInTx atomically unclaims an issue.
// Sets assignee to "" and status to "open".
// Records an "unclaimed" event.
// Only works on issues that have an assignee and status is "open" or "in_progress".
// Returns error if:
//   - Issue is closed (cannot unclaim closed issues)
//   - Issue has no assignee (nothing to unclaim)
func UnclaimIssueInTx(ctx context.Context, tx *sql.Tx, id string, actor string) error {
	return unclaimIssueInTx(ctx, tx, id, actor, "")
}

// UnclaimIssueIfAssigneeInTx atomically releases a claim only while the issue
// is still assigned to expectedAssignee — the compare-and-swap inverse of
// ClaimIssueInTx: a conditional UPDATE ... WHERE id = ? AND assignee = ? with
// RowsAffected as the verdict, so a stale releaser can never clobber a claim
// that has since moved to (or been re-taken by) someone else. On success it
// applies the same transition as UnclaimIssueInTx (assignee cleared, status
// back to open, "unclaimed" event recorded). When the current assignee differs
// from expectedAssignee — including when the issue is no longer assigned at
// all — it returns storage.ErrAssigneeMismatch naming the current holder and
// leaves the row untouched.
func UnclaimIssueIfAssigneeInTx(ctx context.Context, tx *sql.Tx, id string, actor string, expectedAssignee string) error {
	if expectedAssignee == "" {
		return fmt.Errorf("conditional unclaim of %s: expected assignee must not be empty (use UnclaimIssueInTx for an unconditional release)", id)
	}
	return unclaimIssueInTx(ctx, tx, id, actor, expectedAssignee)
}

// unclaimIssueInTx is the shared body: expectedAssignee == "" releases
// unconditionally (legacy bd unclaim), otherwise the UPDATE is a CAS on the
// assignee column.
//
//nolint:gosec // G201: issueTable/eventTable are hardcoded constants
func unclaimIssueInTx(ctx context.Context, tx *sql.Tx, id string, actor string, expectedAssignee string) error {
	// Read current issue
	issueTable := "issues"
	eventTable := "events"

	oldIssue, err := GetIssueInTx(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("failed to get issue for unclaim: %w", err)
	}

	// Validate: cannot unclaim closed issues
	if oldIssue.Status == types.StatusClosed {
		return fmt.Errorf("cannot unclaim closed issue %s", id)
	}

	// Conditional release: a mismatched holder is a loud, typed no-op. The
	// read and the UPDATE below run in the same transaction, so this check and
	// the CAS WHERE clause see the same row state.
	if expectedAssignee != "" && oldIssue.Assignee != expectedAssignee {
		return fmt.Errorf("%w: issue %s is held by %q, expected %q", storage.ErrAssigneeMismatch, id, oldIssue.Assignee, expectedAssignee)
	}

	// Validate: must have an assignee to unclaim
	if oldIssue.Assignee == "" {
		return fmt.Errorf("issue %s is not assigned", id)
	}

	now := time.Now().UTC()

	// Atomic UPDATE: clear assignee and reset status to open. The conditional
	// form pins the row to the expected assignee (CAS); the unconditional form
	// only requires that some assignee is set.
	var result sql.Result
	if expectedAssignee != "" {
		result, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET assignee = '', status = 'open', updated_at = ?
			WHERE id = ? AND assignee = ? AND status IN ('open', 'in_progress')
		`, issueTable), now, id, expectedAssignee)
	} else {
		result, err = tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE %s
			SET assignee = '', status = 'open', updated_at = ?
			WHERE id = ? AND assignee != '' AND status IN ('open', 'in_progress')
		`, issueTable), now, id)
	}
	if err != nil {
		return fmt.Errorf("failed to unclaim issue: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}

	if rowsAffected == 0 {
		if expectedAssignee != "" {
			// The CAS lost: the row no longer matches the expected assignee
			// (belt-and-suspenders — the in-tx precheck above already caught
			// the common case).
			return fmt.Errorf("%w: issue %s is no longer held by %q", storage.ErrAssigneeMismatch, id, expectedAssignee)
		}
		return fmt.Errorf("failed to unclaim issue %s: no matching row", id)
	}

	// Record the unclaim event
	oldData, _ := json.Marshal(oldIssue)
	newUpdates := map[string]interface{}{
		"assignee": "",
		"status":   "open",
	}
	newData, _ := json.Marshal(newUpdates)

	if err := RecordFullEventInTable(ctx, tx, eventTable, id, "unclaimed", actor, string(oldData), string(newData)); err != nil {
		return fmt.Errorf("failed to record unclaim event: %w", err)
	}

	return nil
}
