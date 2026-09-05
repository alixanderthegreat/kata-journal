package kata

import (
	"context"
	"encoding/binary"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

// buildOldBboltFixture creates a bbolt file matching kata-journal's REAL pre-gordian-db layout
// (see MigrateFromBbolt's own doc comment) - not a synthetic shape invented for this test alone,
// so a real cycle's actual JSON encoding is exercised, not a stand-in.
func buildOldBboltFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()

	idKey := func(id uint64) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, id)
		return b
	}

	err = db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucket([]byte("meta"))
		if err != nil {
			return err
		}
		if err := meta.Put([]byte("challenge:proj-a"), []byte("A's challenge")); err != nil {
			return err
		}
		if err := meta.Put([]byte("root_challenge"), []byte("the old legacy challenge")); err != nil {
			return err
		}

		cycles, err := tx.CreateBucket([]byte("cycles"))
		if err != nil {
			return err
		}
		threadA, err := cycles.CreateBucket([]byte("thread-a"))
		if err != nil {
			return err
		}
		c0 := `{"id":0,"kind":"directed","thread":"thread-a","challenge":"A's challenge","target_condition":"t0","obstacles":null,"test":null,"expectations":"","results":"done","deadline":"2026-01-01T00:00:00Z","started_at":"2026-01-01T00:00:00Z","closed_at":"2026-01-02T00:00:00Z"}`
		if err := threadA.Put(idKey(0), []byte(c0)); err != nil {
			return err
		}
		c1 := `{"id":1,"kind":"directed","thread":"thread-a","challenge":"A's challenge","target_condition":"t1","obstacles":null,"test":null,"expectations":"","results":"","deadline":"2026-02-01T00:00:00Z","started_at":"2026-02-01T00:00:00Z"}`
		return threadA.Put(idKey(1), []byte(c1))
	})
	if err != nil {
		t.Fatalf("populate fixture: %v", err)
	}
	return path
}

// TestMigrateFromBbolt proves a real migration works end to end, verified through the Store's
// own public API - not raw key/value checks - matching the discipline this cycle exists to
// enforce (a real automated test, not another manual pass against real data).
func TestMigrateFromBbolt(t *testing.T) {
	oldPath := buildOldBboltFixture(t)

	newPath := filepath.Join(t.TempDir(), "new")
	s, err := Open(newPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	challenges, cycles, err := s.MigrateFromBbolt(oldPath)
	if err != nil {
		t.Fatalf("MigrateFromBbolt: %v", err)
	}
	if challenges != 2 {
		t.Errorf("challenges = %d, want 2", challenges)
	}
	if cycles != 2 {
		t.Errorf("cycles = %d, want 2", cycles)
	}

	ctx := context.Background()

	gotA, err := s.Challenge(ctx, "proj-a")
	if err != nil {
		t.Fatalf("Challenge(proj-a): %v", err)
	}
	if gotA != "A's challenge" {
		t.Errorf("Challenge(proj-a) = %q, want %q", gotA, "A's challenge")
	}

	gotLegacy, err := s.Challenge(ctx, "")
	if err != nil {
		t.Fatalf("Challenge(\"\"): %v", err)
	}
	if gotLegacy != "the old legacy challenge" {
		t.Errorf("Challenge(\"\") = %q, want the migrated root_challenge value", gotLegacy)
	}

	closed, err := s.ClosedCycles(ctx, "thread-a")
	if err != nil {
		t.Fatalf("ClosedCycles: %v", err)
	}
	if len(closed) != 1 || closed[0].ID != 0 || closed[0].Results != "done" {
		t.Fatalf("ClosedCycles = %+v, want exactly cycle 0 with Results %q", closed, "done")
	}

	active, err := s.Active(ctx, "thread-a")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active == nil || active.ID != 1 || active.TargetCondition != "t1" {
		t.Fatalf("Active = %+v, want cycle 1, still open", active)
	}
}

// TestMigrateFromBbolt_RefusesOverwrite proves migrating into a Store that already has
// conflicting data errors out rather than silently overwriting it - obstacle 1's requirement.
func TestMigrateFromBbolt_RefusesOverwrite(t *testing.T) {
	oldPath := buildOldBboltFixture(t)

	newPath := filepath.Join(t.TempDir(), "new")
	s, err := Open(newPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.SetChallenge(context.Background(), "proj-a", "already set here"); err != nil {
		t.Fatalf("SetChallenge: %v", err)
	}

	if _, _, err := s.MigrateFromBbolt(oldPath); err == nil {
		t.Fatal("expected MigrateFromBbolt to refuse overwriting an existing challenge, got nil error")
	}

	got, err := s.Challenge(context.Background(), "proj-a")
	if err != nil {
		t.Fatalf("Challenge(proj-a): %v", err)
	}
	if got != "already set here" {
		t.Fatalf("Challenge(proj-a) = %q, want the pre-existing value untouched", got)
	}
}
