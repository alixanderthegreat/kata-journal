package kata

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Three legacy-format self-healing tests (TestLegacyCorruptedRowSelfHeals,
// TestLegacyObstacleDecodesAsBareString, TestLegacyTestItemResultsDecodesAsBareString) were
// removed here, not adapted - see kata cycle 13 (gordian-db's own project): they existed to
// prove old bbolt-encoded rows (predating certain fields, or written in an older JSON shape)
// still decoded correctly, planted directly via raw bbolt transactions. A gordian-db-backed
// Store has no such legacy bbolt data to migrate or self-heal from, so there is nothing for an
// equivalent test to exercise - this isn't a coverage regression, it's dropping tests whose
// entire premise no longer applies. TestChallengeScopedByProject and TestPruneAutonomous were
// ADAPTED instead (their core assertions are genuine, portable business logic), not removed -
// see their own bodies below for what changed and why.

// TestCycleLifecycle walks one full kata: Start, record an obstacle (which must NOT be required
// for the Test to close), add Test items unrelated to that obstacle, set cycle-level
// Expectations, complete the items, and Close - proving a cycle can have one obstacle and a
// 10-item Test that isn't ultimately meant to overcome that obstacle.
func TestCycleLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	if _, err := s.Start(ctx, thread, "directed", "get sharper", "faster replies", "sluggish, untuned", deadline); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Starting a second cycle while one is active must fail.
	if _, err := s.Start(ctx, thread, "directed", "x", "y", "z", deadline); err == nil {
		t.Fatal("expected error starting a second cycle while one is active")
	}

	if err := s.AddObstacle(ctx, thread, "we have no obstacles"); err != nil {
		t.Fatalf("AddObstacle: %v", err)
	}

	if err := s.SetExpectations(ctx, thread, "measurably faster, shorter replies by the end of this cycle"); err != nil {
		t.Fatalf("SetExpectations: %v", err)
	}

	// Test items deliberately unrelated to the obstacle above - proving
	// there's no 1:1 pairing enforced anywhere in this package.
	if err := s.AddTestItem(ctx, thread, "tune sampling params"); err != nil {
		t.Fatalf("AddTestItem 1: %v", err)
	}
	if err := s.AddTestItem(ctx, thread, "shorten voice examples"); err != nil {
		t.Fatalf("AddTestItem 2: %v", err)
	}

	active, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil {
		t.Fatal("expected an active cycle")
	}
	if len(active.Obstacles) != 1 || len(active.Test) != 2 {
		t.Fatalf("unexpected cycle shape: %+v", active)
	}
	if active.Obstacles[0].RecordedAt.IsZero() {
		t.Fatal("expected AddObstacle to stamp RecordedAt")
	}
	for _, item := range active.Test {
		if item.CreatedAt.IsZero() {
			t.Fatalf("expected AddTestItem to stamp CreatedAt on item %d", item.ID)
		}
		if item.CompletedAt != nil {
			t.Fatalf("expected CompletedAt to be nil before completion on item %d", item.ID)
		}
	}
	if active.Expectations == "" {
		t.Fatal("expected Expectations to be set")
	}
	if active.IsComplete() {
		t.Fatal("cycle should not be complete with pending test items")
	}

	// Closing before all items are done must fail.
	if _, err := s.CloseCycle(ctx, thread, "too early"); err == nil {
		t.Fatal("expected error closing an incomplete cycle")
	}

	// Deadline hasn't passed yet - CloseIfDeadlinePassed must be a no-op.
	closedByDeadline, err := s.CloseIfDeadlinePassed(ctx, thread, "")
	if err != nil {
		t.Fatalf("CloseIfDeadlinePassed (not yet due): %v", err)
	}
	if closedByDeadline != nil {
		t.Fatal("expected no-op before the deadline arrives")
	}

	for _, item := range active.Test {
		if err := s.CompleteItem(ctx, thread, item.ID, "done in test"); err != nil {
			t.Fatalf("CompleteItem %d: %v", item.ID, err)
		}
	}
	completed, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after completing items: %v", err)
	}
	for _, item := range completed.Test {
		if item.CompletedAt == nil {
			t.Fatalf("expected CompleteItem to stamp CompletedAt on item %d", item.ID)
		}
	}

	closed, err := s.CloseCycle(ctx, thread, "faster and shorter, momentum kept")
	if err != nil {
		t.Fatalf("CloseCycle: %v", err)
	}
	if closed.ClosedAt == nil {
		t.Fatal("expected ClosedAt to be set")
	}
	if closed.Results == "" {
		t.Fatal("expected a retrospective result")
	}

	// No active cycle after closing - the next Start should succeed cleanly.
	stillActive, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after close: %v", err)
	}
	if stillActive != nil {
		t.Fatal("expected no active cycle after CloseCycle")
	}

	if _, err := s.Start(ctx, thread, "directed", "next challenge", "next target", "post-cycle state", deadline); err != nil {
		t.Fatalf("Start next cycle: %v", err)
	}
}

// TestDeadlineWithNoResults proves "no results by the deadline is results":
// a cycle with an incomplete Test past its deadline closes anyway, with an
// honest, non-error Results entry recording that nothing was achieved.
func TestDeadlineWithNoResults(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"

	// Deadline already in the past.
	if _, err := s.Start(ctx, thread, "directed", "challenge", "target", "current", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.AddTestItem(ctx, thread, "an item that never gets finished"); err != nil {
		t.Fatalf("AddTestItem: %v", err)
	}

	closed, err := s.CloseIfDeadlinePassed(ctx, thread, "")
	if err != nil {
		t.Fatalf("CloseIfDeadlinePassed: %v", err)
	}
	if closed == nil {
		t.Fatal("expected the cycle to close - its deadline already passed")
	}
	if closed.IsComplete() {
		t.Fatal("test should still be incomplete")
	}
	if closed.Results == "" {
		t.Fatal("expected a non-empty Results even though nothing was achieved")
	}
	t.Logf("results on expiry: %s", closed.Results)
}

// TestDeadlineWithSuppliedResultsOverridesCannedMessage proves that a caller who does have a
// real retrospective at hand isn't forced to accept the generic "no results by the deadline"
// filler - CloseIfDeadlinePassed must store what it's given verbatim instead.
func TestDeadlineWithSuppliedResultsOverridesCannedMessage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"

	// Deadline already in the past.
	if _, err := s.Start(ctx, thread, "directed", "challenge", "target", "current", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.AddTestItem(ctx, thread, "an item that never gets finished"); err != nil {
		t.Fatalf("AddTestItem: %v", err)
	}

	const supplied = "got most of the way there before time ran out - worth resuming next cycle"
	closed, err := s.CloseIfDeadlinePassed(ctx, thread, supplied)
	if err != nil {
		t.Fatalf("CloseIfDeadlinePassed: %v", err)
	}
	if closed == nil {
		t.Fatal("expected the cycle to close - its deadline already passed")
	}
	if closed.Results != supplied {
		t.Fatalf("expected supplied results to override the canned message, got %q", closed.Results)
	}
}

// TestDeadlineWithEmptyTestIsNotFalselyComplete guards a real false-positive found live: a
// cycle whose Test was never built at all (nil, not just incomplete) is vacuously "complete"
// by Cycle.IsComplete's own definition, but CloseIfDeadlinePassed must not mislabel that as
// "Test completed by the deadline." - nothing was actually tested, so the honest record is the
// same "no results" outcome as an incomplete Test.
func TestDeadlineWithEmptyTestIsNotFalselyComplete(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"

	if _, err := s.Start(ctx, thread, "autonomous", "challenge", "target", "current", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Deliberately no AddTestItem call - this cycle never had a Test built.

	closed, err := s.CloseIfDeadlinePassed(ctx, thread, "")
	if err != nil {
		t.Fatalf("CloseIfDeadlinePassed: %v", err)
	}
	if closed == nil {
		t.Fatal("expected the cycle to close - its deadline already passed")
	}
	if closed.Results == "Test completed by the deadline." {
		t.Fatalf("an empty, never-built Test must not be reported as completed: %q", closed.Results)
	}
}

// TestPruneAutonomous seeds a mix that mirrors the real problem: a handful of deliberately
// authored (directed) cycles, a pile of closed autonomous self-check cycles, and one still-active
// autonomous cycle - then confirms PruneAutonomous removes only the excess closed autonomous
// ones, never a directed cycle and never the active one.
func TestPruneAutonomous(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	startAndClose := func(kind string) int64 {
		c, err := s.Start(ctx, thread, kind, "c", "t", "cur", deadline)
		if err != nil {
			t.Fatalf("Start(%s): %v", kind, err)
		}
		if _, err := s.CloseCycle(ctx, thread, "done"); err != nil {
			t.Fatalf("CloseCycle(%s) id=%d: %v", kind, c.ID, err)
		}
		return c.ID
	}

	var directedIDs, autonomousIDs []int64
	for i := 0; i < 2; i++ {
		directedIDs = append(directedIDs, startAndClose("directed"))
	}
	for i := 0; i < 5; i++ {
		autonomousIDs = append(autonomousIDs, startAndClose("autonomous"))
	}
	active, err := s.Start(ctx, thread, "autonomous", "c", "t", "cur", deadline)
	if err != nil {
		t.Fatalf("Start active: %v", err)
	}

	const keep = 2
	if err := s.PruneAutonomous(ctx, thread, keep); err != nil {
		t.Fatalf("PruneAutonomous: %v", err)
	}

	// Adapted from raw-bbolt-bucket verification to the public API (kata cycle 13): what
	// remains after pruning is exactly the active cycle plus everything ClosedCycles reports.
	remaining := map[int64]bool{}
	if active != nil {
		remaining[active.ID] = true
	}
	closedAfterPrune, err := s.ClosedCycles(ctx, thread)
	if err != nil {
		t.Fatalf("ClosedCycles: %v", err)
	}
	for _, c := range closedAfterPrune {
		remaining[c.ID] = true
	}

	for _, id := range directedIDs {
		if !remaining[id] {
			t.Errorf("directed cycle %d was pruned - must never happen", id)
		}
	}
	if !remaining[active.ID] {
		t.Errorf("active cycle %d was pruned - must never happen", active.ID)
	}
	// The newest `keep` autonomous cycles (the last two of autonomousIDs) must survive;
	// the older three must be gone.
	survivors := autonomousIDs[len(autonomousIDs)-keep:]
	pruned := autonomousIDs[:len(autonomousIDs)-keep]
	for _, id := range survivors {
		if !remaining[id] {
			t.Errorf("recent autonomous cycle %d should have survived pruning", id)
		}
	}
	for _, id := range pruned {
		if remaining[id] {
			t.Errorf("old autonomous cycle %d should have been pruned", id)
		}
	}
}

// TestClosedCycles proves ClosedCycles returns every closed cycle for a thread, newest first,
// excluding the still-active one - the full history behind LastClosed's single most-recent
// entry. Also exercises LastClosed itself (previously untested) since it shares the exact same
// seed data and ordering guarantee.
func TestClosedCycles(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	var closedIDs []int64
	for i := 0; i < 3; i++ {
		c, err := s.Start(ctx, thread, "directed", "c", "t", "cur", deadline)
		if err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
		if _, err := s.CloseCycle(ctx, thread, fmt.Sprintf("result %d", i)); err != nil {
			t.Fatalf("CloseCycle %d: %v", i, err)
		}
		closedIDs = append(closedIDs, c.ID)
	}
	active, err := s.Start(ctx, thread, "directed", "c", "t", "cur", deadline)
	if err != nil {
		t.Fatalf("Start active: %v", err)
	}

	closed, err := s.ClosedCycles(ctx, thread)
	if err != nil {
		t.Fatalf("ClosedCycles: %v", err)
	}
	if len(closed) != len(closedIDs) {
		t.Fatalf("expected %d closed cycles, got %d", len(closedIDs), len(closed))
	}
	for i, c := range closed {
		wantID := closedIDs[len(closedIDs)-1-i] // newest first
		if c.ID != wantID {
			t.Fatalf("closed[%d].ID = %d, want %d (newest-first order)", i, c.ID, wantID)
		}
		if c.ID == active.ID {
			t.Fatalf("ClosedCycles returned the still-active cycle %d", active.ID)
		}
	}

	lastClosed, err := s.LastClosed(ctx, thread)
	if err != nil {
		t.Fatalf("LastClosed: %v", err)
	}
	if lastClosed == nil || lastClosed.ID != closed[0].ID {
		t.Fatalf("LastClosed disagrees with ClosedCycles[0]: got %+v, want id %d", lastClosed, closed[0].ID)
	}
}

// TestUpdateScopedByThread guards against a real bug: two different threads independently
// reaching the same numeric id (expected and common - each thread counts from its own zero, see
// insert) must never cross-contaminate on update.
func TestUpdateScopedByThread(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	deadline := time.Now().Add(time.Hour)

	threadA, threadB := "thread-a", "thread-b"

	// Both threads start from id 0 independently - deliberately made to collide.
	if _, err := s.Start(ctx, threadA, "autonomous", "cA", "tA", "curA", deadline); err != nil {
		t.Fatalf("Start A: %v", err)
	}
	if _, err := s.Start(ctx, threadB, "autonomous", "cB", "tB", "curB", deadline); err != nil {
		t.Fatalf("Start B: %v", err)
	}

	// Mutate only A's cycle.
	if err := s.SetCurrentCondition(ctx, threadA, "A moved forward"); err != nil {
		t.Fatalf("SetCurrentCondition A: %v", err)
	}

	// B's cycle must be completely untouched by A's update.
	activeB, err := s.Active(ctx, threadB)
	if err != nil {
		t.Fatalf("Active B: %v", err)
	}
	if activeB == nil {
		t.Fatal("expected B's cycle to still exist")
	}
	if activeB.Thread != threadB {
		t.Fatalf("B's cycle identity was corrupted: thread=%q", activeB.Thread)
	}
	if activeB.CurrentCondition != "curB" {
		t.Fatalf("B's cycle content was overwritten by A's update: current=%q", activeB.CurrentCondition)
	}

	activeA, err := s.Active(ctx, threadA)
	if err != nil {
		t.Fatalf("Active A: %v", err)
	}
	if activeA.CurrentCondition != "A moved forward" {
		t.Fatalf("A's own update didn't apply: current=%q", activeA.CurrentCondition)
	}
}

// TestEditObstacle proves EditObstacle rewrites text in place, stamps EditedAt, leaves
// RecordedAt untouched, and rejects an out-of-range index without touching the cycle.
func TestEditObstacle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	if _, err := s.Start(ctx, thread, "directed", "c", "t", "cur", deadline); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.AddObstacle(ctx, thread, "a typo-ridden obstalce"); err != nil {
		t.Fatalf("AddObstacle: %v", err)
	}
	before, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	recordedAt := before.Obstacles[0].RecordedAt
	if before.Obstacles[0].EditedAt != nil {
		t.Fatal("expected EditedAt to be nil before any edit")
	}

	if err := s.EditObstacle(ctx, thread, 99, "out of range"); err == nil {
		t.Fatal("expected error editing an out-of-range obstacle index")
	}

	if err := s.EditObstacle(ctx, thread, 0, "a corrected obstacle"); err != nil {
		t.Fatalf("EditObstacle: %v", err)
	}
	after, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after edit: %v", err)
	}
	if after.Obstacles[0].Text != "a corrected obstacle" {
		t.Fatalf("expected edited text, got %q", after.Obstacles[0].Text)
	}
	if after.Obstacles[0].EditedAt == nil {
		t.Fatal("expected EditObstacle to stamp EditedAt")
	}
	if !after.Obstacles[0].RecordedAt.Equal(recordedAt) {
		t.Fatalf("expected RecordedAt to stay untouched, got %v want %v", after.Obstacles[0].RecordedAt, recordedAt)
	}
}

// TestEditTestItem proves EditTestItem rewrites a still-open item's text and stamps EditedAt,
// but refuses once that item is Done - editing it after completion would rewrite the wording
// its Results was actually recorded against.
func TestEditTestItem(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	if _, err := s.Start(ctx, thread, "directed", "c", "t", "cur", deadline); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.AddTestItem(ctx, thread, "tset the thign"); err != nil {
		t.Fatalf("AddTestItem: %v", err)
	}

	if err := s.EditTestItem(ctx, thread, 99, "no such item"); err == nil {
		t.Fatal("expected error editing a nonexistent test item id")
	}

	if err := s.EditTestItem(ctx, thread, 0, "test the thing"); err != nil {
		t.Fatalf("EditTestItem: %v", err)
	}
	active, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active.Test[0].Text != "test the thing" {
		t.Fatalf("expected edited text, got %q", active.Test[0].Text)
	}
	if active.Test[0].EditedAt == nil {
		t.Fatal("expected EditTestItem to stamp EditedAt")
	}

	if err := s.CompleteItem(ctx, thread, 0, "done"); err != nil {
		t.Fatalf("CompleteItem: %v", err)
	}
	if err := s.EditTestItem(ctx, thread, 0, "trying to rewrite history"); err == nil {
		t.Fatal("expected EditTestItem to refuse editing an already-completed item")
	}
	untouched, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after refused edit: %v", err)
	}
	if untouched.Test[0].Text != "test the thing" {
		t.Fatalf("refused edit must not change the stored text, got %q", untouched.Test[0].Text)
	}
}

// TestCompleteItemAppendsResultsHistory reproduces a real incident from this project's own
// dogfood use (kata cycle 61, test item 0): calling CompleteItem a second time on an
// already-Done item used to silently overwrite the first result with no trace - the opposite of
// this package's own "don't sanitize the record" posture. It must now append instead, preserving
// both, with CompletedAt staying pinned to the first completion.
func TestCompleteItemAppendsResultsHistory(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	deadline := time.Now().Add(time.Hour)

	if _, err := s.Start(ctx, thread, "directed", "c", "t", "cur", deadline); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.AddTestItem(ctx, thread, "probe something"); err != nil {
		t.Fatalf("AddTestItem: %v", err)
	}

	if err := s.CompleteItem(ctx, thread, 0, "first, low-effort pass"); err != nil {
		t.Fatalf("CompleteItem (first): %v", err)
	}
	first, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after first complete: %v", err)
	}
	if len(first.Test[0].Results) != 1 {
		t.Fatalf("expected 1 results entry after first completion, got %d: %+v", len(first.Test[0].Results), first.Test[0].Results)
	}
	firstCompletedAt := first.Test[0].CompletedAt
	if firstCompletedAt == nil {
		t.Fatal("expected CompletedAt to be stamped on first completion")
	}

	if err := s.CompleteItem(ctx, thread, 0, "second, verified pass"); err != nil {
		t.Fatalf("CompleteItem (second): %v", err)
	}
	second, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after second complete: %v", err)
	}
	if len(second.Test[0].Results) != 2 {
		t.Fatalf("expected 2 results entries after second completion, got %d: %+v", len(second.Test[0].Results), second.Test[0].Results)
	}
	if second.Test[0].Results[0].Text != "first, low-effort pass" {
		t.Fatalf("first result was overwritten instead of preserved: %+v", second.Test[0].Results)
	}
	if second.Test[0].Results[1].Text != "second, verified pass" {
		t.Fatalf("expected second result appended, got %+v", second.Test[0].Results)
	}
	if !second.Test[0].CompletedAt.Equal(*firstCompletedAt) {
		t.Fatalf("expected CompletedAt to stay at the first completion time, got %v (was %v)", second.Test[0].CompletedAt, firstCompletedAt)
	}
}

// TestChallengeScopedByProject proves Challenge is a per-project setting: setting one project's
// Challenge must not affect another's, and an unset project reads back "".
//
// ADAPTED (kata cycle 13): the original also proved a legacy flat rootChallengeKey (planted
// directly via raw bbolt) still worked as a fallback for any project that hadn't set its own
// Challenge yet - a bbolt-migration-compatibility concern with no equivalent for a gordian-db
// store that has no such legacy data. That part is dropped; the core per-project-isolation
// assertion below is unchanged in substance.
func TestChallengeScopedByProject(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()

	for _, project := range []string{"project-a", "project-b"} {
		got, err := s.Challenge(ctx, project)
		if err != nil {
			t.Fatalf("Challenge(%q): %v", project, err)
		}
		if got != "" {
			t.Fatalf("Challenge(%q) = %q, want empty before anything is set", project, got)
		}
	}

	if err := s.SetChallenge(ctx, "project-a", "project A's own challenge"); err != nil {
		t.Fatalf("SetChallenge: %v", err)
	}

	gotA, err := s.Challenge(ctx, "project-a")
	if err != nil {
		t.Fatalf("Challenge(project-a): %v", err)
	}
	if gotA != "project A's own challenge" {
		t.Fatalf("Challenge(project-a) = %q, want the newly-set project value", gotA)
	}

	gotB, err := s.Challenge(ctx, "project-b")
	if err != nil {
		t.Fatalf("Challenge(project-b): %v", err)
	}
	if gotB != "" {
		t.Fatalf("Challenge(project-b) = %q, want it still empty, unaffected by project-a's write", gotB)
	}
}
