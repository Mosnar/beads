package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/beads/internal/storage"
	"github.com/steveyegge/beads/internal/storage/sqlite"
	"github.com/steveyegge/beads/internal/types"
)

// chunkRecordingStore records every CreateIssuesWithFullOptions call as a
// snapshot (issue rows plus the dependencies attached at call time), so tests
// can assert on transaction boundaries. failOnCall simulates a mid-import
// failure at the Nth call (1-based; 0 = never fail).
type chunkRecordingStore struct {
	storage.DoltStorage
	batches    [][]*types.Issue
	calls      int
	failOnCall int
}

func (f *chunkRecordingStore) GetIssuesByIDs(_ context.Context, _ []string) ([]*types.Issue, error) {
	return nil, nil
}

func (f *chunkRecordingStore) CreateIssuesWithFullOptions(_ context.Context, issues []*types.Issue, _ string, _ storage.BatchCreateOptions) error {
	f.calls++
	if f.failOnCall != 0 && f.calls == f.failOnCall {
		return errors.New("simulated chunk failure")
	}
	snapshot := make([]*types.Issue, len(issues))
	for i, issue := range issues {
		cp := *issue
		cp.Dependencies = append([]*types.Dependency(nil), issue.Dependencies...)
		snapshot[i] = &cp
	}
	f.batches = append(f.batches, snapshot)
	return nil
}

func setImportChunkSize(t *testing.T, n int) {
	t.Helper()
	old := importChunkSize
	importChunkSize = n
	t.Cleanup(func() { importChunkSize = old })
}

func setImportProgressBuffer(t *testing.T) *bytes.Buffer {
	t.Helper()
	old := importProgress
	buf := &bytes.Buffer{}
	importProgress = buf
	t.Cleanup(func() { importProgress = old })
	return buf
}

// recordImportPauses replaces the inter-chunk sleep with a counter so tests
// run at full speed while still asserting the pause is issued.
func recordImportPauses(t *testing.T) *int {
	t.Helper()
	old := importPause
	count := 0
	importPause = func(time.Duration) { count++ }
	t.Cleanup(func() { importPause = old })
	return &count
}

func chunkTestIssues(n int) []*types.Issue {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	issues := make([]*types.Issue, n)
	for i := range issues {
		issues[i] = &types.Issue{
			ID:        fmt.Sprintf("bd-chunk%02d", i+1),
			Title:     fmt.Sprintf("chunk issue %d", i+1),
			UpdatedAt: base,
		}
		issues[i].SetDefaults()
	}
	return issues
}

// An import at or below the chunk size must keep today's semantics exactly:
// one CreateIssuesWithFullOptions call, dependencies inline, one transaction.
func TestImportIssuesCoreSingleBatchAtOrBelowChunkSize(t *testing.T) {
	setImportChunkSize(t, 4)
	recordImportPauses(t)
	issues := chunkTestIssues(4)
	issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[3].ID, Type: types.DepBlocks}}

	store := &chunkRecordingStore{}
	result, err := importIssuesCore(context.Background(), "", store, issues, ImportOptions{SkipPrefixValidation: true})
	if err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 transaction for a small import", store.calls)
	}
	if len(store.batches[0]) != 4 {
		t.Fatalf("batch size = %d, want 4", len(store.batches[0]))
	}
	foundDep := false
	for _, issue := range store.batches[0] {
		if issue.ID == issues[0].ID && len(issue.Dependencies) == 1 {
			foundDep = true
		}
	}
	if !foundDep {
		t.Fatalf("small import must keep dependencies inline in the single batch")
	}
	if result.Created != 4 {
		t.Fatalf("Created = %d, want 4", result.Created)
	}
}

// A large import must be split into bounded transactions so the write lock is
// released between chunks instead of being held for the whole batch.
func TestImportIssuesCoreChunksLargeImports(t *testing.T) {
	setImportChunkSize(t, 3)
	recordImportPauses(t)
	progress := setImportProgressBuffer(t)
	issues := chunkTestIssues(8)

	store := &chunkRecordingStore{}
	result, err := importIssuesCore(context.Background(), "", store, issues, ImportOptions{SkipPrefixValidation: true})
	if err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 3 {
		t.Fatalf("calls = %d, want 3 bounded transactions (3+3+2)", store.calls)
	}
	wantSizes := []int{3, 3, 2}
	seen := map[string]int{}
	for i, batch := range store.batches {
		if len(batch) != wantSizes[i] {
			t.Fatalf("batch %d size = %d, want %d", i, len(batch), wantSizes[i])
		}
		for _, issue := range batch {
			seen[issue.ID]++
		}
	}
	for _, issue := range issues {
		if seen[issue.ID] != 1 {
			t.Fatalf("issue %s written %d times, want exactly once", issue.ID, seen[issue.ID])
		}
	}
	if result.Created != 8 {
		t.Fatalf("Created = %d, want 8", result.Created)
	}
	if got := progress.String(); !strings.Contains(got, "8/8") {
		t.Fatalf("progress output missing final count, got %q", got)
	}
}

// Exactly chunk-size and chunk-size+1 imports: no empty trailing chunk, and
// the boundary issue lands in a second transaction.
func TestImportIssuesCoreChunkBoundaries(t *testing.T) {
	setImportChunkSize(t, 3)
	recordImportPauses(t)

	store := &chunkRecordingStore{}
	if _, err := importIssuesCore(context.Background(), "", store, chunkTestIssues(3), ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 1 {
		t.Fatalf("calls = %d, want 1 for an exactly-chunk-size import", store.calls)
	}

	store = &chunkRecordingStore{}
	if _, err := importIssuesCore(context.Background(), "", store, chunkTestIssues(4), ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 2 {
		t.Fatalf("calls = %d, want 2 for a chunk-size+1 import", store.calls)
	}
	if got := []int{len(store.batches[0]), len(store.batches[1])}; got[0] != 3 || got[1] != 1 {
		t.Fatalf("batch sizes = %v, want [3 1]", got)
	}
}

// Readiness-affecting dependencies must land in the same transaction as the
// dependent's row, whatever order the JSONL puts the rows in: the import
// reorders rows so every (acyclic) blocking target lands in the same or an
// earlier chunk, and the edge rides inline with the row. No separate
// dependency pass may exist for them — a dependency pass is a window in which
// a concurrent reader sees the row without its edges.
func TestImportChunkedBlockingDepsLandInSameTransactionAsRow(t *testing.T) {
	setImportChunkSize(t, 3)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	issues := chunkTestIssues(7)
	// Forward reference across the chunk boundary in file order: 1 -> 7.
	issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[6].ID, Type: types.DepBlocks}}
	// Backward reference: 5 -> 1.
	issues[4].Dependencies = []*types.Dependency{{IssueID: issues[4].ID, DependsOnID: issues[0].ID, Type: types.DepBlocks}}

	store := &chunkRecordingStore{}
	if _, err := importIssuesCore(context.Background(), "", store, issues, ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 3 {
		t.Fatalf("calls = %d, want 3 row chunks and NO separate dependency pass", store.calls)
	}

	// Every dependency must ride in its owner's row batch, and its target must
	// already exist by the end of that batch (same or earlier batch).
	batchOf := map[string]int{}
	for b, batch := range store.batches {
		for _, issue := range batch {
			batchOf[issue.ID] = b
		}
	}
	wantDeps := map[string]string{
		issues[0].ID: issues[6].ID,
		issues[4].ID: issues[0].ID,
	}
	got := map[string]string{}
	for _, batch := range store.batches {
		for _, issue := range batch {
			for _, dep := range issue.Dependencies {
				got[issue.ID] = dep.DependsOnID
				tb, ok := batchOf[dep.DependsOnID]
				if !ok {
					t.Fatalf("dependency target %s never written", dep.DependsOnID)
				}
				if tb > batchOf[issue.ID] {
					t.Fatalf("issue %s (batch %d) carries an edge to %s (batch %d): target does not exist when the edge commits",
						issue.ID, batchOf[issue.ID], dep.DependsOnID, tb)
				}
			}
		}
	}
	for id, target := range wantDeps {
		if got[id] != target {
			t.Fatalf("edge %s -> %s not written inline (got %q)", id, target, got[id])
		}
	}
	// The caller's issues must come back with dependencies intact so a retry
	// of the same slice still carries them.
	if len(issues[0].Dependencies) != 1 || len(issues[4].Dependencies) != 1 {
		t.Fatalf("original issues lost their dependencies after import")
	}
}

// A failure mid-import must surface as an error naming the committed prefix,
// stop issuing further transactions, and leave the input re-runnable.
func TestImportIssuesCoreChunkedMidFailureLeavesCommittedPrefix(t *testing.T) {
	setImportChunkSize(t, 3)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	issues := chunkTestIssues(8)
	issues[7].Dependencies = []*types.Dependency{{IssueID: issues[7].ID, DependsOnID: issues[0].ID, Type: types.DepBlocks}}

	store := &chunkRecordingStore{failOnCall: 2}
	_, err := importIssuesCore(context.Background(), "", store, issues, ImportOptions{SkipPrefixValidation: true})
	if err == nil {
		t.Fatalf("importIssuesCore succeeded, want mid-chunk failure to surface")
	}
	if !strings.Contains(err.Error(), "3 issues already committed") {
		t.Fatalf("error %q does not name the committed prefix", err)
	}
	if !strings.Contains(err.Error(), "re-run") {
		t.Fatalf("error %q does not tell the user the import is re-runnable", err)
	}
	if store.calls != 2 {
		t.Fatalf("calls = %d, want to stop after the failing chunk", store.calls)
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 3 {
		t.Fatalf("committed prefix = %d batches, want exactly the first chunk", len(store.batches))
	}
	if len(issues[7].Dependencies) != 1 {
		t.Fatalf("failure path lost the caller's dependencies; retry would drop edges")
	}
}

// The bounded transactions must not run back-to-back: a chunked import that
// re-takes the write lock microseconds after each commit starves every
// concurrent bd operation for the whole import (SQLite busy-polling has no
// fairness queue). A pause must separate every adjacent pair of import
// transactions, including the boundary into the deferred-dependency pass.
func TestImportChunkedPausesBetweenChunkTransactions(t *testing.T) {
	setImportChunkSize(t, 3)
	setImportProgressBuffer(t)
	pauses := recordImportPauses(t)
	issues := chunkTestIssues(8)
	// A non-blocking forward reference forces a deferred-dependency pass, so
	// the count also covers the phase boundary. 3 row chunks + 1 dep pass.
	issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[7].ID, Type: types.DepRelated}}

	store := &chunkRecordingStore{}
	if _, err := importIssuesCore(context.Background(), "", store, issues, ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if store.calls != 4 {
		t.Fatalf("calls = %d, want 3 row chunks + 1 deferred-dependency pass", store.calls)
	}
	if *pauses != store.calls-1 {
		t.Fatalf("pauses = %d, want one between every adjacent pair of transactions (%d)", *pauses, store.calls-1)
	}
	if importInterChunkPause <= 0 {
		t.Fatalf("importInterChunkPause = %v, want a positive gap for lock fairness", importInterChunkPause)
	}
}

// failNthCreateStore wraps a real store and fails the Nth batch-create call,
// simulating a crash mid-import against a real engine.
type failNthCreateStore struct {
	storage.DoltStorage
	calls      int
	failOnCall int
}

func (f *failNthCreateStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	f.calls++
	if f.calls == f.failOnCall {
		return errors.New("simulated crash")
	}
	return f.DoltStorage.CreateIssuesWithFullOptions(ctx, issues, actor, opts)
}

func provisionChunkStore(t *testing.T) storage.DoltStorage {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Provision(ctx, filepath.Join(t.TempDir(), "beads.db"))
	if err != nil {
		t.Fatalf("provision sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.SetConfig(ctx, "issue_prefix", "bd"); err != nil {
		t.Fatalf("seed issue_prefix: %v", err)
	}
	return store
}

// End-to-end against the real SQLite engine: a mid-import failure leaves a
// committed, queryable prefix, and re-running the same import converges —
// every row present, cross-chunk dependencies wired, no duplicated events.
func TestImportChunkedRealStoreResumeConverges(t *testing.T) {
	setImportChunkSize(t, 5)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	ctx := context.Background()
	store := provisionChunkStore(t)

	makeIssues := func() []*types.Issue {
		issues := chunkTestIssues(12)
		// Blocking forward reference in file order: 1 -> 11.
		issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[10].ID, Type: types.DepBlocks}}
		return issues
	}

	failing := &failNthCreateStore{DoltStorage: store, failOnCall: 2}
	_, err := importIssuesCore(ctx, "", failing, makeIssues(), ImportOptions{SkipPrefixValidation: true})
	if err == nil {
		t.Fatalf("importIssuesCore succeeded, want simulated crash at chunk 2")
	}

	// Some rows are committed and queryable, others absent: the transactions
	// really are bounded.
	committed, absent := 0, 0
	for i := 1; i <= 12; i++ {
		if _, err := store.GetIssue(ctx, fmt.Sprintf("bd-chunk%02d", i)); err == nil {
			committed++
		} else {
			absent++
		}
	}
	if committed != 5 || absent != 7 {
		t.Fatalf("after crash at chunk 2: committed=%d absent=%d, want exactly the first chunk (5) durable", committed, absent)
	}

	// Re-run the identical import: it must converge.
	result, err := importIssuesCore(ctx, "", store, makeIssues(), ImportOptions{SkipPrefixValidation: true})
	if err != nil {
		t.Fatalf("re-run importIssuesCore: %v", err)
	}
	if result.Created != 12 {
		t.Fatalf("re-run Created = %d, want all 12 rows accounted for", result.Created)
	}
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("bd-chunk%02d", i)
		if _, err := store.GetIssue(ctx, id); err != nil {
			t.Fatalf("issue %s missing after re-run: %v", id, err)
		}
	}
	deps, err := store.GetDependencies(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "bd-chunk11" {
		t.Fatalf("cross-chunk dependency not wired, got %#v", deps)
	}
	// Rows upserted twice must not accrue duplicate created events.
	for _, id := range []string{"bd-chunk01", "bd-chunk02"} {
		events, err := store.GetEvents(ctx, id, 50)
		if err != nil {
			t.Fatalf("GetEvents(%s): %v", id, err)
		}
		created := 0
		for _, e := range events {
			if e.EventType == types.EventCreated {
				created++
			}
		}
		if created != 1 {
			t.Fatalf("%s has %d created events after resume, want 1", id, created)
		}
	}
}

// No freeze point of a chunked import may expose a blocked bead as ready:
// whenever an imported row is visible to a concurrent reader, the blocking
// edges its import file declares must be visible too. This is the red-team
// ready-window finding: the previous two-phase layout committed every row
// dep-less first, so `bd ready` mid-import offered blocked work for dispatch.
func TestImportChunkedNoReadyWindowAtAnyFreezePoint(t *testing.T) {
	setImportChunkSize(t, 5)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	ctx := context.Background()

	makeIssues := func() []*types.Issue {
		issues := chunkTestIssues(12)
		// bd-chunk01 is BLOCKED by bd-chunk11 (forward ref in file order).
		issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[10].ID, Type: types.DepBlocks}}
		return issues
	}

	// Learn how many transactions a full import issues.
	full := provisionChunkStore(t)
	counting := &failNthCreateStore{DoltStorage: full}
	if _, err := importIssuesCore(ctx, "", counting, makeIssues(), ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("full import: %v", err)
	}
	totalCalls := counting.calls
	if totalCalls < 2 {
		t.Fatalf("totalCalls = %d, want a chunked import", totalCalls)
	}
	ready, err := full.GetReadyWork(ctx, types.WorkFilter{Limit: 50})
	if err != nil {
		t.Fatalf("GetReadyWork: %v", err)
	}
	for _, issue := range ready {
		if issue.ID == "bd-chunk01" {
			t.Fatalf("bd-chunk01 ready after a full import; it is blocked by open bd-chunk11")
		}
	}

	// Freeze the store at every possible transaction boundary and check the
	// invariant: row visible => blocking edge visible => not ready.
	for failAt := 1; failAt <= totalCalls; failAt++ {
		store := provisionChunkStore(t)
		failing := &failNthCreateStore{DoltStorage: store, failOnCall: failAt}
		if _, err := importIssuesCore(ctx, "", failing, makeIssues(), ImportOptions{SkipPrefixValidation: true}); err == nil {
			t.Fatalf("freeze point %d: import unexpectedly succeeded", failAt)
		}
		if _, err := store.GetIssue(ctx, "bd-chunk01"); err != nil {
			continue // row not committed yet: nothing to observe
		}
		deps, err := store.GetDependencies(ctx, "bd-chunk01")
		if err != nil {
			t.Fatalf("freeze point %d: GetDependencies: %v", failAt, err)
		}
		if len(deps) == 0 {
			t.Fatalf("freeze point %d: bd-chunk01 committed without its blocking edge — ready window", failAt)
		}
		ready, err := store.GetReadyWork(ctx, types.WorkFilter{Limit: 50})
		if err != nil {
			t.Fatalf("freeze point %d: GetReadyWork: %v", failAt, err)
		}
		for _, issue := range ready {
			if issue.ID == "bd-chunk01" {
				t.Fatalf("freeze point %d: blocked bead bd-chunk01 offered as ready work mid-import", failAt)
			}
		}
	}
}

// raceInjectingStore simulates a concurrent writer landing between import
// transactions: before the Nth CreateIssuesWithFullOptions call it updates an
// issue directly, bumping its updated_at — exactly what a concurrent
// `bd update` or a gc claim does in the windows the chunking opens up.
type raceInjectingStore struct {
	storage.DoltStorage
	calls        int
	injectBefore int
	injectID     string
	injected     bool
	t            *testing.T
}

func (r *raceInjectingStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	r.calls++
	if r.calls == r.injectBefore && !r.injected {
		r.injected = true
		if err := r.DoltStorage.UpdateIssue(ctx, r.injectID, map[string]interface{}{"assignee": "concurrent-racer"}, "racer"); err != nil {
			r.t.Fatalf("inject concurrent update: %v", err)
		}
	}
	return r.DoltStorage.CreateIssuesWithFullOptions(ctx, issues, actor, opts)
}

// A concurrent update to a row between its commit and the deferred-dependency
// pass must not drop the import's edges for it. This is the red-team #1
// finding: the old dependency pass resubmitted the full row with
// RejectStaleUpserts, so the now-newer stored row stale-rejected the resubmit
// and the engine dropped its deps — silently, permanently (re-runs pre-filter
// the row away). The dependency pass must wire edges for every row phase 1
// accepted, regardless of later row-level updates, and must not clobber the
// concurrent write.
func TestImportChunkedConcurrentUpdateKeepsDeferredDeps(t *testing.T) {
	setImportChunkSize(t, 5)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	ctx := context.Background()
	store := provisionChunkStore(t)

	makeIssues := func() []*types.Issue {
		issues := chunkTestIssues(12)
		// A non-blocking forward edge stays deferred (related edges do not
		// affect readiness, so the ordering pass leaves file order alone):
		// this is the shape that still crosses the inter-transaction window.
		issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[10].ID, Type: types.DepRelated}}
		return issues
	}

	// 12 issues at chunk size 5 = 3 row chunks (calls 1-3); the deferred
	// dependency pass is call 4. Inject the concurrent update just before it.
	racing := &raceInjectingStore{DoltStorage: store, injectBefore: 4, injectID: "bd-chunk01", t: t}
	result, err := importIssuesCore(ctx, "", racing, makeIssues(), ImportOptions{SkipPrefixValidation: true})
	if err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if !racing.injected {
		t.Fatalf("race was never injected (calls=%d); test harness broken", racing.calls)
	}

	// The concurrent write survives: the dependency pass must not rewrite rows.
	got, err := store.GetIssue(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("bd-chunk01 not committed: %v", err)
	}
	if got.Assignee != "concurrent-racer" {
		t.Fatalf("assignee = %q, want the concurrent update preserved", got.Assignee)
	}

	// The import's edge survives: phase 1 accepted the row, so its deferred
	// deps must be wired even though the stored row is now newer.
	deps, err := store.GetDependencies(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "bd-chunk11" {
		t.Fatalf("deferred edge dropped after concurrent update: deps = %#v", deps)
	}

	// And the reporting must not misclassify the row: its row IS committed.
	for _, id := range result.StaleSkippedIDs {
		if id == "bd-chunk01" {
			t.Fatalf("bd-chunk01 reported stale-skipped, but its row was imported in phase 1")
		}
	}
	if result.Created != 12 {
		t.Fatalf("Created = %d, want 12", result.Created)
	}
}

// The red-team #1 repro shape verbatim, for blocking edges: a claim-like
// update racing the import must leave the blocking edge intact. With ordered
// inline wiring the row and its edge commit atomically, so no interleaving of
// row-level updates can separate them.
func TestImportChunkedConcurrentUpdateKeepsBlockingDeps(t *testing.T) {
	setImportChunkSize(t, 5)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	ctx := context.Background()
	store := provisionChunkStore(t)

	makeIssues := func() []*types.Issue {
		issues := chunkTestIssues(12)
		issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[10].ID, Type: types.DepBlocks}}
		return issues
	}

	// The blocker's row (bd-chunk11) lands before bd-chunk01's chunk. Update
	// it between chunks — the interleaving that used to poison the dep pass.
	racing := &raceInjectingStore{DoltStorage: store, injectBefore: 3, injectID: "bd-chunk11", t: t}
	if _, err := importIssuesCore(ctx, "", racing, makeIssues(), ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if !racing.injected {
		t.Fatalf("race was never injected (calls=%d); test harness broken", racing.calls)
	}
	deps, err := store.GetDependencies(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].ID != "bd-chunk11" {
		t.Fatalf("blocking edge missing after concurrent update: deps = %#v", deps)
	}

	// Re-running the identical import stays convergent and keeps the edge.
	if _, err := importIssuesCore(ctx, "", store, makeIssues(), ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("re-run importIssuesCore: %v", err)
	}
	deps, err = store.GetDependencies(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("GetDependencies after re-run: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("blocking edge lost on re-run: deps = %#v", deps)
	}
}

// The other half of the stale-snapshot invariant (bd-578h9.8): when the row
// write itself was stale-rejected in phase 1 (a local update landed between
// the pre-filter read and the row's chunk), the snapshot's deferred deps must
// stay out too. This is why the dependency pass cannot simply run everything
// with ConflictSkip: it must be restricted to rows phase 1 accepted.
func TestImportChunkedStaleRejectedRowKeepsDeferredDepsOut(t *testing.T) {
	setImportChunkSize(t, 5)
	recordImportPauses(t)
	setImportProgressBuffer(t)
	ctx := context.Background()
	store := provisionChunkStore(t)
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	// Seed bd-chunk01 with an OLDER snapshot so the incoming row passes the
	// pre-filter (incoming strictly newer than local at read time).
	seed := &types.Issue{ID: "bd-chunk01", Title: "seeded", UpdatedAt: base.Add(-time.Hour)}
	seed.SetDefaults()
	if _, err := importIssuesCore(ctx, "", store, []*types.Issue{seed}, ImportOptions{SkipPrefixValidation: true}); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	issues := chunkTestIssues(12)
	issues[0].Dependencies = []*types.Dependency{{IssueID: issues[0].ID, DependsOnID: issues[10].ID, Type: types.DepRelated}}

	// Bump bd-chunk01 to "now" after the pre-filter read but before its row
	// chunk: the in-transaction stale guard rejects the row write, so the
	// whole snapshot — deferred deps included — must stay out.
	racing := &raceInjectingStore{DoltStorage: store, injectBefore: 1, injectID: "bd-chunk01", t: t}
	result, err := importIssuesCore(ctx, "", racing, issues, ImportOptions{SkipPrefixValidation: true})
	if err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if !racing.injected {
		t.Fatalf("race was never injected; test harness broken")
	}

	deps, err := store.GetDependencies(ctx, "bd-chunk01")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("stale-rejected row's deferred deps were wired anyway: %#v (violates bd-578h9.8)", deps)
	}
	found := false
	for _, id := range result.StaleSkippedIDs {
		if id == "bd-chunk01" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bd-chunk01 not reported stale-skipped; StaleSkippedIDs = %v", result.StaleSkippedIDs)
	}
}

// timestampingStore records when each import transaction starts and returns,
// so the availability test can reason about "strictly inside the import".
type timestampingStore struct {
	storage.DoltStorage
	mu     sync.Mutex
	starts []time.Time
	ends   []time.Time
}

func (s *timestampingStore) CreateIssuesWithFullOptions(ctx context.Context, issues []*types.Issue, actor string, opts storage.BatchCreateOptions) error {
	s.mu.Lock()
	s.starts = append(s.starts, time.Now())
	s.mu.Unlock()
	err := s.DoltStorage.CreateIssuesWithFullOptions(ctx, issues, actor, opts)
	s.mu.Lock()
	s.ends = append(s.ends, time.Now())
	s.mu.Unlock()
	return err
}

// The point of chunking is availability: concurrent bd operations must keep
// succeeding WHILE a large import runs, not merely after it. This probes a
// live chunked import from a second store handle (a separate connection pool
// on the same database file — the same file-lock domain a separate bd process
// contends on) and requires reads and writes to complete strictly inside the
// import window. The red-team measurement showed bounded transactions alone
// do not deliver this: back-to-back chunks starve waiters; the inter-chunk
// pause is what makes this test pass.
func TestImportChunkedConcurrentAvailability(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-second availability probe; skipped in -short")
	}
	setImportChunkSize(t, 100)
	setImportProgressBuffer(t)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "beads.db")
	importSide, err := sqlite.Provision(ctx, dbPath)
	if err != nil {
		t.Fatalf("provision import store: %v", err)
	}
	defer importSide.Close()
	if err := importSide.SetConfig(ctx, "issue_prefix", "bd"); err != nil {
		t.Fatalf("seed issue_prefix: %v", err)
	}
	probeSide, err := sqlite.Provision(ctx, dbPath)
	if err != nil {
		t.Fatalf("provision probe store: %v", err)
	}
	defer probeSide.Close()

	stop := make(chan struct{})
	var probeMu sync.Mutex
	var readOK, writeOK []time.Time
	var probeErrs []error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // reader probe
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := probeSide.GetReadyWork(ctx, types.WorkFilter{Limit: 5}); err != nil {
				probeMu.Lock()
				probeErrs = append(probeErrs, fmt.Errorf("ready probe: %w", err))
				probeMu.Unlock()
			} else {
				probeMu.Lock()
				readOK = append(readOK, time.Now())
				probeMu.Unlock()
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	go func() { // writer probe
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			issue := &types.Issue{ID: fmt.Sprintf("bd-p%04d", n), Title: "probe"}
			issue.SetDefaults()
			if err := probeSide.CreateIssue(ctx, issue, "probe"); err != nil {
				probeMu.Lock()
				probeErrs = append(probeErrs, fmt.Errorf("create probe %d: %w", n, err))
				probeMu.Unlock()
			} else {
				probeMu.Lock()
				writeOK = append(writeOK, time.Now())
				probeMu.Unlock()
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	ts := &timestampingStore{DoltStorage: importSide}
	_, err = importIssuesCore(ctx, "", ts, chunkTestIssues(1000), ImportOptions{SkipPrefixValidation: true})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("importIssuesCore: %v", err)
	}
	if len(ts.starts) < 4 {
		t.Fatalf("import issued %d transactions, want a genuinely chunked run", len(ts.starts))
	}

	for _, err := range probeErrs {
		t.Errorf("concurrent bd operation failed during import: %v", err)
	}
	// "Strictly inside": after the first transaction committed and before the
	// last one began — the region the old single-transaction import blacked out.
	windowStart := ts.ends[0]
	windowEnd := ts.starts[len(ts.starts)-1]
	if !windowEnd.After(windowStart) {
		t.Fatalf("degenerate import window [%v, %v]", windowStart, windowEnd)
	}
	inWindow := func(times []time.Time) int {
		n := 0
		for _, at := range times {
			if at.After(windowStart) && at.Before(windowEnd) {
				n++
			}
		}
		return n
	}
	if got := inWindow(readOK); got == 0 {
		t.Errorf("no read completed strictly inside the import window (%d total reads)", len(readOK))
	}
	if got := inWindow(writeOK); got == 0 {
		t.Errorf("no write completed strictly inside the import window (%d total writes)", len(writeOK))
	}
}
