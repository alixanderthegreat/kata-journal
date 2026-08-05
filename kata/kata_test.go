package kata

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

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

	remaining := map[int64]bool{}
	if err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, thread)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(k, v []byte) error {
			remaining[idFromKey(k)] = true
			return nil
		})
	}); err != nil {
		t.Fatalf("query remaining: %v", err)
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

// TestLegacyCorruptedRowSelfHeals reproduces the exact shape found live: an entry stored under
// the correct thread bucket, but whose JSON payload's embedded Thread field disagrees (planted
// directly via bbolt, mimicking what the pre-fix update() bug actually left behind). A read must
// report the trusted thread, not the stale payload, and any subsequent write must self-heal the
// payload rather than perpetuate the lie.
func TestLegacyCorruptedRowSelfHeals(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	trueThread := "thread-a"
	corruptedData := []byte(`{"id":223,"kind":"autonomous","thread":"thread-b","challenge":"Play.","target_condition":"t","current_condition":"c","deadline":"2099-01-01T00:00:00Z","started_at":"2026-01-01T00:00:00Z"}`)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := threadBucketCreate(tx, trueThread)
		if err != nil {
			return err
		}
		return bucket.Put(idKey(223), corruptedData)
	}); err != nil {
		t.Fatalf("plant corrupted row: %v", err)
	}

	active, err := s.Active(ctx, trueThread)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil {
		t.Fatal("expected the planted row to be found")
	}
	if active.Thread != trueThread {
		t.Fatalf("read did not self-heal: got thread=%q, want %q", active.Thread, trueThread)
	}

	if err := s.SetCurrentCondition(ctx, trueThread, "healed"); err != nil {
		t.Fatalf("SetCurrentCondition: %v", err)
	}
	var data []byte
	if err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, trueThread)
		if bucket == nil {
			return fmt.Errorf("thread bucket missing")
		}
		v := bucket.Get(idKey(223))
		if v == nil {
			return fmt.Errorf("planted entry missing")
		}
		data = append([]byte(nil), v...)
		return nil
	}); err != nil {
		t.Fatalf("query healed row: %v", err)
	}
	if !strings.Contains(string(data), `"thread":"thread-a"`) {
		t.Fatalf("write did not self-heal the stored payload: %s", data)
	}
}

// TestLegacyObstacleDecodesAsBareString reproduces a real pre-existing row shape: every cycle
// closed before Obstacle gained its own type stored "obstacles" as a bare JSON array of strings
// (`["text"]`), not objects. Those rows must keep decoding cleanly - with a zero RecordedAt, not
// an error and not a fabricated timestamp - and a subsequent write must upgrade the stored shape
// without losing the original text.
func TestLegacyObstacleDecodesAsBareString(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "kata.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	thread := "thread-a"
	legacyData := []byte(`{"id":0,"kind":"directed","thread":"thread-a","challenge":"Play.","target_condition":"t","current_condition":"c","obstacles":["an old obstacle, recorded before timestamps existed"],"deadline":"2099-01-01T00:00:00Z","started_at":"2026-01-01T00:00:00Z"}`)
	if err := s.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := threadBucketCreate(tx, thread)
		if err != nil {
			return err
		}
		return bucket.Put(idKey(0), legacyData)
	}); err != nil {
		t.Fatalf("plant legacy row: %v", err)
	}

	active, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil {
		t.Fatal("expected the planted legacy row to be found")
	}
	if len(active.Obstacles) != 1 {
		t.Fatalf("expected exactly one decoded obstacle, got %+v", active.Obstacles)
	}
	if active.Obstacles[0].Text != "an old obstacle, recorded before timestamps existed" {
		t.Fatalf("legacy obstacle text lost on decode: %+v", active.Obstacles[0])
	}
	if !active.Obstacles[0].RecordedAt.IsZero() {
		t.Fatalf("expected a zero RecordedAt for a legacy obstacle, got %v", active.Obstacles[0].RecordedAt)
	}

	// A new obstacle added afterward must get a real timestamp, sitting alongside the
	// zero-timestamped legacy one without disturbing it.
	if err := s.AddObstacle(ctx, thread, "a new obstacle, recorded now"); err != nil {
		t.Fatalf("AddObstacle: %v", err)
	}
	updated, err := s.Active(ctx, thread)
	if err != nil {
		t.Fatalf("Active after AddObstacle: %v", err)
	}
	if len(updated.Obstacles) != 2 {
		t.Fatalf("expected two obstacles after adding one, got %+v", updated.Obstacles)
	}
	if !updated.Obstacles[0].RecordedAt.IsZero() {
		t.Fatalf("legacy obstacle's RecordedAt should remain zero, got %v", updated.Obstacles[0].RecordedAt)
	}
	if updated.Obstacles[1].RecordedAt.IsZero() {
		t.Fatal("expected the newly added obstacle to have a real RecordedAt")
	}
}
