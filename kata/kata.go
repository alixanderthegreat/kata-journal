// Package kata is a small, structured planning layer for tracking a cycle of directed work in
// its canonical form: Challenge, Target Condition, Current Condition, Obstacles, Test,
// Expectations, Results - with a Deadline, because no results by the deadline is itself a
// result, not an error state.
//
//   - Challenge: the why.
//   - Target Condition: the goal, validated against the Challenge.
//   - Current Condition: self-assessment.
//   - Obstacles: a flat, unordered list of what's in the way - context and
//     awareness (a causality map), NOT a checklist paired 1:1 with the Test.
//   - Test: an ordered set of items constructed holistically to advance
//     Current Condition toward Target Condition. Informed by the obstacles,
//     not indexed to them - a cycle can run a 10-item Test around zero named
//     obstacles.
//   - Expectations: what the Test as a WHOLE is expected to achieve - one
//     cycle-level field, not one per item.
//   - Results: the retrospective, recorded when the cycle closes.
//   - Deadline: when the cycle closes regardless of Test completion. If
//     nothing was achieved by then, the Results field records that plainly -
//     "no results by the deadline" is a valid, honest result, not a failure
//     to be blocked on.
//
// Cycles are scoped by a thread - a plain string key. One project's db file (see Open) is
// already its own scope by virtue of being its own file, so thread exists for the layer below
// that: several parallel lines of work sharing one db file. Most projects only ever need one,
// the CLI's own default. Backed by gordian-db (github.com/alixanderthegreat/gordian-db), a
// pure-Go, pebble-based key/value store - swapped in from bbolt in kata cycle 12/13 of
// gordian-db's own project. See Store's doc comment for what changed and why.
package kata

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	gordian "github.com/alixanderthegreat/gordian-db"
)

// TestItem is one item in a cycle's Test - one of the moves constructed to advance Current
// Condition toward Target Condition, not a response to a specific Obstacle (see package doc: no
// 1:1 pairing between Test and Obstacles). No per-item expectation field either - see package
// doc: expectations live at the cycle level, describing what the Test as a whole is meant to
// achieve, not what each item individually proves. CreatedAt/CompletedAt exist so a cycle's Test
// reads as a real timeline, not just an ID-ordered list - a legacy item decoded from before
// these fields existed comes back with a zero CreatedAt and a nil CompletedAt, which is an
// honest "unknown," not a fabricated guess. CompletedAt stamps the FIRST completion only (see
// CompleteItem) - each individual completion's own time lives on its ResultsEntry instead.
type TestItem struct {
	ID          int            `json:"id"`
	Text        string         `json:"text"`
	Done        bool           `json:"done"`
	Results     ResultsHistory `json:"results,omitempty"` // every CompleteItem call appends - see CompleteItem
	CreatedAt   time.Time      `json:"created_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	EditedAt    *time.Time     `json:"edited_at,omitempty"` // set by EditTestItem - nil means never edited
}

// ResultsEntry is one recorded outcome for a TestItem - see ResultsHistory.
type ResultsEntry struct {
	Text       string    `json:"text"`
	RecordedAt time.Time `json:"recorded_at,omitempty"`
}

// ResultsHistory is the ordered, oldest-first list of every outcome CompleteItem has recorded for
// one TestItem. Found live (kata cycle 61, test item 0, this project's own dogfood use) that a
// second, better CompleteItem call on an already-Done item silently clobbered the first honest
// result with no trace - the opposite of EditTestItem's deliberate refusal to let Text be
// rewritten after completion, and a real loss given the whole package's "don't sanitize the
// record" posture (see package doc on Results/Deadline). CompleteItem now appends here instead of
// overwriting a single string, so a correction is additional history, not erasure.
//
// Decodes from either its current array shape or a bare JSON string, the shape every TestItem's
// Results was stored in before this existed - a legacy value decodes as one entry with a zero
// RecordedAt (honest "unknown," not a fabricated guess - same convention Obstacle.UnmarshalJSON
// already established for the same reason).
type ResultsHistory []ResultsEntry

func (rh *ResultsHistory) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		if text == "" {
			*rh = nil
			return nil
		}
		*rh = ResultsHistory{{Text: text}}
		return nil
	}
	type alias ResultsHistory
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*rh = ResultsHistory(a)
	return nil
}

// Obstacle is one entry in a cycle's flat, unordered obstacle list (see
// package doc) - a plain string plus when it was actually noticed.
type Obstacle struct {
	Text       string     `json:"text"`
	RecordedAt time.Time  `json:"recorded_at,omitempty"`
	EditedAt   *time.Time `json:"edited_at,omitempty"` // set by EditObstacle - nil means never edited
}

// UnmarshalJSON accepts either the current object shape ({"text":...,"recorded_at":...}) or a
// bare JSON string - every obstacle recorded before this field existed is stored as a plain
// string inside its cycle's JSON blob, and those existing rows must keep decoding cleanly rather
// than erroring or silently losing history. A legacy obstacle decodes with a zero RecordedAt
// (never backfilled with a fake time) rather than pretending to know when it wasn't recorded.
func (o *Obstacle) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		o.Text = text
		o.RecordedAt = time.Time{}
		return nil
	}
	type alias Obstacle
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*o = Obstacle(a)
	return nil
}

// Cycle is one full kata: open from Start until closed, either by
// CloseCycle (Test complete, explicit results) or CloseIfDeadlinePassed
// (Deadline reached regardless of completion - see package doc). Obstacles
// is intentionally flat and unordered.
type Cycle struct {
	ID               int64      `json:"id"`
	Kind             string     `json:"kind,omitempty"` // "autonomous" (self-driven cycling) or "directed" (deliberately started) - set once, at Start, by which caller started it
	Thread           string     `json:"thread"`
	Challenge        string     `json:"challenge"`
	TargetCondition  string     `json:"target_condition"`
	CurrentCondition string     `json:"current_condition"`
	Obstacles        []Obstacle `json:"obstacles"`
	Test             []TestItem `json:"test"`
	Expectations     string     `json:"expectations"`
	Results          string     `json:"results"` // set on close - the retrospective
	Deadline         time.Time  `json:"deadline"`
	StartedAt        time.Time  `json:"started_at"`
	ClosedAt         *time.Time `json:"closed_at,omitempty"`
}

// IsComplete reports whether every Test item is done. An empty Test counts as complete (nothing
// left to finish) - consistent with a cycle built around zero obstacles, and by the same logic,
// one that hasn't had any test items added yet.
func (c *Cycle) IsComplete() bool {
	for _, item := range c.Test {
		if !item.Done {
			return false
		}
	}
	return true
}

// Store persists cycles in a gordian-db store - a single embedded, pure-Go key/value store (see
// github.com/alixanderthegreat/gordian-db), no SQL or embeddings needed for "what's the active
// cycle for this thread."
//
// Layout: gordian-db's Store has one flat keyspace with prefix Scan, not bbolt's nested
// buckets - this package's own key encoding (see cycleKey/cyclePrefix/challengeKey) gives
// thread and category isolation that bbolt's bucket boundaries used to provide for free. A tag
// byte separates key categories (Challenge vs Cycle) so they can never collide with each other,
// and within a category a 2-byte big-endian LENGTH PREFIX precedes each variable-length string
// (project or thread name) before its own bytes - this is what makes the encoding
// collision-safe: two different strings can never produce ambiguous overlapping key ranges the
// way plain concatenation could (e.g. thread "a" vs thread "ab"). Every Cycle field (including
// its own id) lives inside the JSON value, same as before - the key is purely for ordering and
// lookup, decode always trusts its own arguments over whatever the JSON payload claims (see
// decode's own doc comment).
//
// This Store starts fresh - it has no legacy bbolt-format data to migrate or self-heal from
// (the previous bbolt-backed Store's legacy-compatibility paths - a root Challenge fallback,
// and self-healing for corrupted/pre-existing rows - had no equivalent need here and were not
// ported; see kata-journal's kata cycle 13 for the migration this replaced).
type Store struct {
	db *gordian.Store

	// mu serializes every read-modify-write against this store. A Cycle read followed by a
	// write is not atomic on its own, so any caller sharing one Store across goroutines needs
	// this - coarse on purpose, this is a single-operator tool, not a multi-tenant service.
	mu sync.Mutex
}

// Key encoding - see Store's own doc comment for why a tag byte + length prefix is what makes
// this collision-safe, not plain concatenation.
const (
	tagChallenge byte = 0x01
	tagCycle     byte = 0x02
)

func lengthPrefixed(buf []byte, s string) []byte {
	buf = binary.BigEndian.AppendUint16(buf, uint16(len(s)))
	return append(buf, s...)
}

// challengeKey derives a project's own Challenge entry - project is the same repo/cwd identity
// used elsewhere (see the CLI's projectIdentity()), kept independent of thread so several threads
// within one project (e.g. one per agent) share the same Challenge rather than each getting their
// own fork of it.
func challengeKey(project string) []byte {
	return lengthPrefixed([]byte{tagChallenge}, project)
}

// cyclePrefix is the Scan prefix covering every cycle for thread - the db file itself is already
// a project's own scope (see Open), so this is purely the layer below that: several parallel
// lines of work sharing one file.
func cyclePrefix(thread string) []byte {
	return lengthPrefixed([]byte{tagCycle}, thread)
}

// cycleKey encodes one cycle's full key: cyclePrefix(thread) followed by its id as 8 bytes,
// big-endian - Scan's key order is raw byte order, so a fixed-width big-endian encoding is what
// makes key order match numeric id order (a decimal string encoding would not: "10" sorts before
// "9").
func cycleKey(thread string, id int64) []byte {
	key := cyclePrefix(thread)
	idBuf := make([]byte, 8)
	binary.BigEndian.PutUint64(idBuf, uint64(id))
	return append(key, idBuf...)
}

func idFromCycleKey(key []byte) int64 {
	return int64(binary.BigEndian.Uint64(key[len(key)-8:]))
}

func Open(dbPath string) (*Store, error) {
	db, err := gordian.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open gordian-db store: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// MigrateFromBbolt migrates every Challenge and Cycle out of an OLD bbolt-backed kata-journal
// database (bboltPath) into this Store, using gordian-db's WalkBbolt primitive against the
// pre-gordian-db bucket layout: a top-level "meta" bucket (keys "challenge:<project>", plus the
// legacy single "root_challenge" slot from before per-project scoping existed) and a top-level
// "cycles" bucket (one nested bucket per thread, entries keyed by an 8-byte big-endian id -
// bbolt's own layout, not this Store's - see cycleKey/challengeKey for what a gordian-db-backed
// Store actually stores now).
//
// root_challenge is migrated under the empty-string project key - preserving it losslessly
// without inventing a new fallback-lookup behavior this Store doesn't otherwise have; nothing
// currently resolves the empty-string project in real use, so it sits there as an inert,
// recoverable historical artifact rather than being silently dropped.
//
// Read-only against the source throughout (WalkBbolt itself opens bboltPath with bbolt's own
// ReadOnly option - migration can never mutate or corrupt the file being migrated). Refuses to
// overwrite: before writing any entry, checks whether the destination key already exists and
// returns an error naming exactly what collided rather than silently clobbering real data -
// this is meant to migrate INTO an empty or non-conflicting Store, not merge on top of live data
// sharing the same project/thread+id identity.
//
// Returns how many challenges and cycles were actually migrated before any error - a partial
// migration is reported as a real error (and a real partial count), never silently completed
// short or swallowed.
func (s *Store) MigrateFromBbolt(bboltPath string) (challenges, cycles int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	err = gordian.WalkBbolt(bboltPath, func(bucketPath [][]byte, key, value []byte) error {
		if len(bucketPath) == 0 {
			return fmt.Errorf("unexpected empty bucket path")
		}
		switch string(bucketPath[0]) {
		case "meta":
			if len(bucketPath) != 1 {
				return fmt.Errorf("unexpected meta bucket depth %d", len(bucketPath))
			}
			k := string(key)
			var project string
			switch {
			case k == "root_challenge":
				project = ""
			case strings.HasPrefix(k, "challenge:"):
				project = strings.TrimPrefix(k, "challenge:")
			default:
				return fmt.Errorf("unrecognized meta key %q - refusing to silently drop it", k)
			}
			dk := challengeKey(project)
			if _, ok, getErr := s.db.Get(dk); getErr != nil {
				return getErr
			} else if ok {
				return fmt.Errorf("challenge for project %q already exists in destination - refusing to overwrite", project)
			}
			if putErr := s.db.Put(dk, append([]byte(nil), value...)); putErr != nil {
				return putErr
			}
			challenges++

		case "cycles":
			if len(bucketPath) != 2 {
				return fmt.Errorf("unexpected cycles bucket depth %d (path %q) - schema assumption violated", len(bucketPath), bucketPath)
			}
			thread := string(bucketPath[1])
			if len(key) != 8 {
				return fmt.Errorf("unexpected cycle key length %d for thread %q - expected 8-byte id", len(key), thread)
			}
			id := int64(binary.BigEndian.Uint64(key))
			dk := cycleKey(thread, id)
			if _, ok, getErr := s.db.Get(dk); getErr != nil {
				return getErr
			} else if ok {
				return fmt.Errorf("cycle %d for thread %q already exists in destination - refusing to overwrite", id, thread)
			}
			if putErr := s.db.Put(dk, append([]byte(nil), value...)); putErr != nil {
				return putErr
			}
			cycles++

		default:
			return fmt.Errorf("unrecognized top-level bucket %q - refusing to silently drop it", string(bucketPath[0]))
		}
		return nil
	})
	return challenges, cycles, err
}

// OpenOrMigrate opens (or creates) a Store at path, transparently self-migrating first if path
// is itself an OLD-FORMAT bbolt database (a regular file, not a directory) rather than a
// gordian-db-backed store - archiving the old file aside (renamed via os.Rename, NEVER deleted,
// timestamped so it's obviously recoverable) before creating and populating the fresh store via
// MigrateFromBbolt.
//
// migrated reports whether a self-migration actually happened. false (with a nil error) covers
// two legitimate no-op cases, not failures: path doesn't exist yet (brand new install - nothing
// to migrate), or path is already a directory (an existing gordian-db store, or something else
// entirely - either way, not an old bbolt file this can act on).
//
// This exists specifically for the explicit, zero-argument `kata migrate` command - see cycle
// 16 (gordian-db's own project): main() cannot use this for every verb's normal Open(), since
// that would make migration silent/automatic, the opposite of the "explicit and visible" design
// this project deliberately chose. Only the dedicated migrate command should trigger it.
func OpenOrMigrate(path string) (store *Store, migrated bool, challenges, cycles int, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if !os.IsNotExist(statErr) {
			return nil, false, 0, 0, fmt.Errorf("stat %s: %w", path, statErr)
		}
		s, err := Open(path)
		return s, false, 0, 0, err
	}
	if info.IsDir() {
		s, err := Open(path)
		return s, false, 0, 0, err
	}

	archivePath := path + ".bbolt-archive-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, archivePath); err != nil {
		return nil, false, 0, 0, fmt.Errorf("archive old database: %w", err)
	}
	s, err := Open(path)
	if err != nil {
		return nil, false, 0, 0, fmt.Errorf("open fresh store after archiving (old data safe at %s): %w", archivePath, err)
	}
	challenges, cycles, err = s.MigrateFromBbolt(archivePath)
	if err != nil {
		s.Close()
		return nil, false, 0, 0, fmt.Errorf("migrate archived database (archived at %s, not deleted): %w", archivePath, err)
	}
	return s, true, challenges, cycles, nil
}

// SetChallenge persists one project's Challenge, keyed by project (see challengeKey) - a
// one-time (or deliberately-changed) project-wide setting, not a per-cycle or per-thread
// argument. Overwrites whatever was there before for that project; a different project's
// Challenge, in the same db file or otherwise, is untouched.
func (s *Store) SetChallenge(ctx context.Context, project, challenge string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Put(challengeKey(project), []byte(challenge))
}

// Challenge returns one project's Challenge as set by SetChallenge, or "" if it's never been set
// for that project.
func (s *Store) Challenge(ctx context.Context, project string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	v, ok, err := s.db.Get(challengeKey(project))
	if err != nil {
		return "", fmt.Errorf("query challenge: %w", err)
	}
	if !ok {
		return "", nil
	}
	return string(v), nil
}

// scanCyclesLocked returns every cycle for thread, in ascending id order (gordian-db's Scan
// order) - assumes the caller already holds mu. The bbolt-backed Store did this via a cursor
// walking a nested bucket; gordian-db's flat keyspace + prefix Scan makes a full-range scan the
// natural equivalent, consistent with this package's own original design philosophy (see
// PruneAutonomous's and ClosedCycles's original doc comments: "never a lot of rows for a
// single-operator tool... simpler than maintaining a separate index").
func (s *Store) scanCyclesLocked(thread string) ([]*Cycle, error) {
	var cycles []*Cycle
	var decodeErr error
	if err := s.db.Scan(cyclePrefix(thread), func(key, value []byte) bool {
		c, err := decode(idFromCycleKey(key), thread, value)
		if err != nil {
			decodeErr = err
			return false
		}
		cycles = append(cycles, c)
		return true
	}); err != nil {
		return nil, err
	}
	if decodeErr != nil {
		return nil, decodeErr
	}
	return cycles, nil
}

// Active returns the current open cycle (ClosedAt == nil) for thread, or nil if there isn't one
// - a fresh thread simply hasn't started its first kata yet.
func (s *Store) Active(ctx context.Context, thread string) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activeLocked(ctx, thread)
}

// activeLocked is Active's real body, assuming the caller already holds mu - used by other
// locked methods below so they can read the active cycle without a reentrant Lock (sync.Mutex
// isn't reentrant).
//
// Start refuses to open a new cycle while one is already active, so within a thread's history at
// most one cycle is ever open at a time, and it's always the newest (highest id) one - the
// moment the newest entry turns out to already be closed, there is no active cycle at all.
func (s *Store) activeLocked(ctx context.Context, thread string) (*Cycle, error) {
	cycles, err := s.scanCyclesLocked(thread)
	if err != nil {
		return nil, fmt.Errorf("query active cycle: %w", err)
	}
	if len(cycles) == 0 {
		return nil, nil
	}
	last := cycles[len(cycles)-1]
	if last.ClosedAt == nil {
		return last, nil
	}
	return nil, nil
}

// LastClosed returns the most recently closed cycle for thread, or nil if none has ever closed -
// used to seed a new cycle's Challenge/Target from the previous one's retrospective ("beginning
// next kata now").
func (s *Store) LastClosed(ctx context.Context, thread string) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cycles, err := s.scanCyclesLocked(thread)
	if err != nil {
		return nil, fmt.Errorf("query last closed cycle: %w", err)
	}
	for i := len(cycles) - 1; i >= 0; i-- {
		if cycles[i].ClosedAt != nil {
			return cycles[i], nil
		}
	}
	return nil, nil
}

// ClosedCycles returns every closed cycle for thread, newest first - the full history behind
// LastClosed's single most-recent one, for browsing back further than one cycle.
func (s *Store) ClosedCycles(ctx context.Context, thread string) ([]*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cycles, err := s.scanCyclesLocked(thread)
	if err != nil {
		return nil, fmt.Errorf("query closed cycles: %w", err)
	}
	var results []*Cycle
	for i := len(cycles) - 1; i >= 0; i-- {
		if cycles[i].ClosedAt != nil {
			results = append(results, cycles[i])
		}
	}
	return results, nil
}

// PruneAutonomous deletes every CLOSED autonomous cycle for thread beyond the most recent keep -
// ambient self-check cycling that piles up over time, not deliberately authored (directed) work.
// Directed cycles and the currently active one (ClosedAt == nil) are never touched, by
// construction: only entries that decode with Kind == "autonomous" AND a non-nil ClosedAt are
// ever candidates.
func (s *Store) PruneAutonomous(ctx context.Context, thread string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cycles, err := s.scanCyclesLocked(thread)
	if err != nil {
		return fmt.Errorf("query cycles for prune: %w", err)
	}

	seenAutonomous := 0
	for i := len(cycles) - 1; i >= 0; i-- {
		c := cycles[i]
		if c.ClosedAt == nil || c.Kind != "autonomous" {
			continue
		}
		seenAutonomous++
		if seenAutonomous > keep {
			if err := s.db.Delete(cycleKey(thread, c.ID)); err != nil {
				return fmt.Errorf("delete pruned cycle %d: %w", c.ID, err)
			}
		}
	}
	return nil
}

// Start opens a new cycle for thread. Errors if one is already active - close it first. That's
// a deliberate constraint, not an oversight: cycles are a linear sequence ("beginning next kata
// now"), not concurrent threads-of-execution (no relation to the thread parameter's own name).
func (s *Store) Start(ctx context.Context, thread, kind, challenge, targetCondition, currentCondition string, deadline time.Time) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	active, err := s.activeLocked(ctx, thread)
	if err != nil {
		return nil, err
	}
	if active != nil {
		return nil, fmt.Errorf("cycle %d already active for thread %q - close it first", active.ID, thread)
	}

	c := &Cycle{
		Kind:             kind,
		Thread:           thread,
		Challenge:        challenge,
		TargetCondition:  targetCondition,
		CurrentCondition: currentCondition,
		Deadline:         deadline,
		StartedAt:        time.Now(),
	}

	if err := s.insert(ctx, c); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Store) insert(ctx context.Context, c *Cycle) error {
	cycles, err := s.scanCyclesLocked(c.Thread)
	if err != nil {
		return err
	}
	id := int64(0)
	if len(cycles) > 0 {
		id = cycles[len(cycles)-1].ID + 1
	}
	c.ID = id
	data, err := encode(c)
	if err != nil {
		return err
	}
	return s.db.Put(cycleKey(c.Thread, id), data)
}

// update scopes its write by thread+id, not id alone - the same numeric id legitimately exists
// across many different threads (each thread counts from its own zero, see insert), so this
// must never guess which thread a bare id belongs to.
func (s *Store) update(ctx context.Context, thread string, c *Cycle) error {
	c.Thread = thread
	data, err := encode(c)
	if err != nil {
		return err
	}
	return s.db.Put(cycleKey(thread, c.ID), data)
}

// SetCurrentCondition updates the active cycle's self-assessment.
func (s *Store) SetCurrentCondition(ctx context.Context, thread, current string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	c.CurrentCondition = current
	return s.update(ctx, thread, c)
}

// AddObstacle appends to the active cycle's flat obstacle list - context,
// not a checklist item (see package doc).
func (s *Store) AddObstacle(ctx context.Context, thread, obstacle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	c.Obstacles = append(c.Obstacles, Obstacle{Text: obstacle, RecordedAt: time.Now()})
	return s.update(ctx, thread, c)
}

// EditObstacle rewrites the text of one entry in the active cycle's Obstacle list, addressed by
// its position in that list (Obstacles carry no id of their own - see Obstacle). Sets EditedAt
// rather than silently overwriting: a later reader (human or agent) should be able to tell an
// obstacle was revised after RecordedAt, not just trust the current text as if it were original -
// an honest audit trail without going as far as keeping full version history.
func (s *Store) EditObstacle(ctx context.Context, thread string, index int, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	if index < 0 || index >= len(c.Obstacles) {
		return fmt.Errorf("obstacle index %d out of range (cycle %d has %d obstacle(s))", index, c.ID, len(c.Obstacles))
	}
	now := time.Now()
	c.Obstacles[index].Text = text
	c.Obstacles[index].EditedAt = &now
	return s.update(ctx, thread, c)
}

// EditTestItem rewrites the text of one item in the active cycle's Test, addressed by id. Refuses
// once the item is Done: Results is the record of what actually happened against that item's
// wording at the time it was completed, so rewriting the item's Text afterward would be exactly
// the kind of revisionist history this whole edit path exists to avoid - not "hard to fix a typo"
// but "hard to quietly rewrite what was tested." Same EditedAt audit trail as EditObstacle for the
// still-open case. Note the asymmetry with CompleteItem, which is deliberate: Text describes what
// was tested and must stay fixed once real results exist against it, but Results itself is
// allowed to grow (see ResultsHistory) - a correction there is a second real event, not a rewrite.
func (s *Store) EditTestItem(ctx context.Context, thread string, itemID int, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	for i := range c.Test {
		if c.Test[i].ID != itemID {
			continue
		}
		if c.Test[i].Done {
			return fmt.Errorf("test item %d already completed - editing it now would rewrite history; its Results is the record of what actually happened", itemID)
		}
		now := time.Now()
		c.Test[i].Text = text
		c.Test[i].EditedAt = &now
		return s.update(ctx, thread, c)
	}
	return fmt.Errorf("test item %d not found in cycle %d", itemID, c.ID)
}

// AddTestItem appends one item to the active cycle's Test - a move toward Target Condition, not
// a response to a specific Obstacle (see package doc: Test isn't indexed 1:1 to Obstacles).
func (s *Store) AddTestItem(ctx context.Context, thread, text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	item := TestItem{ID: c.nextID(), Text: text, CreatedAt: time.Now()}
	c.Test = append(c.Test, item)
	return s.update(ctx, thread, c)
}

// nextID picks the next test item id from the highest one currently
// present - simpler than tracking a separate counter across encode/decode.
func (c *Cycle) nextID() int {
	max := -1
	for _, item := range c.Test {
		if item.ID > max {
			max = item.ID
		}
	}
	return max + 1
}

// SetExpectations sets what the active cycle's Test, as a whole, is
// expected to achieve - a single cycle-level field (see package doc).
func (s *Store) SetExpectations(ctx context.Context, thread, expectations string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	c.Expectations = expectations
	return s.update(ctx, thread, c)
}

// SetDeadline updates the active cycle's Deadline - e.g. when a caller supplies a real time
// estimate alongside Expectations, after Start opened the cycle with only a placeholder.
func (s *Store) SetDeadline(ctx context.Context, thread string, deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	c.Deadline = deadline
	return s.update(ctx, thread, c)
}

// CompleteItem marks one Test item done by id, appending a new ResultsEntry - calling this again
// on an item that's already Done does NOT overwrite the previous result, it adds another one (see
// ResultsHistory's own doc comment for why this matters and the live incident that found it).
// CompletedAt is only stamped on the first completion; it marks when the item first became Done,
// not when its Results were last touched - each entry's own RecordedAt covers that.
func (s *Store) CompleteItem(ctx context.Context, thread string, itemID int, results string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return err
	}
	found := false
	now := time.Now()
	for i := range c.Test {
		if c.Test[i].ID == itemID {
			c.Test[i].Done = true
			c.Test[i].Results = append(c.Test[i].Results, ResultsEntry{Text: results, RecordedAt: now})
			if c.Test[i].CompletedAt == nil {
				c.Test[i].CompletedAt = &now
			}
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("test item %d not found in cycle %d", itemID, c.ID)
	}
	return s.update(ctx, thread, c)
}

// CloseCycle closes the active cycle with explicit results. Errors if the
// Test isn't complete yet (see Cycle.IsComplete) - this is the "finished
// early/on time" path. For the "deadline arrived regardless" path, see
// CloseIfDeadlinePassed.
func (s *Store) CloseCycle(ctx context.Context, thread, results string) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.requireActiveLocked(ctx, thread)
	if err != nil {
		return nil, err
	}
	if !c.IsComplete() {
		return nil, fmt.Errorf("cycle %d has incomplete test items - can't close yet", c.ID)
	}
	now := time.Now()
	c.ClosedAt = &now
	c.Results = results
	if err := s.update(ctx, thread, c); err != nil {
		return nil, err
	}
	return c, nil
}

// CloseIfDeadlinePassed closes the active cycle if its Deadline has passed, regardless of
// whether the Test is complete - "no results by the deadline is results": an expired deadline
// with nothing achieved isn't a blocked/error state, it's itself a valid, recorded outcome.
// Returns nil, nil if there's no active cycle or its deadline hasn't arrived yet - safe to call
// on every tick.
//
// results, if non-empty, is stored verbatim as the retrospective - a deadline expiring doesn't
// mean nothing happened, and a caller who does have something real to report shouldn't be
// overridden by the canned message below. Pass "" to fall back to that honest default.
func (s *Store) CloseIfDeadlinePassed(ctx context.Context, thread, results string) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	c, err := s.activeLocked(ctx, thread)
	if err != nil {
		return nil, err
	}
	if c == nil || time.Now().Before(c.Deadline) {
		return nil, nil
	}

	now := time.Now()
	c.ClosedAt = &now
	switch {
	case results != "":
		c.Results = results
	// len(c.Test) > 0 matters here, not just IsComplete() - an empty Test is vacuously
	// "complete" by that method's own doc comment, which would mislabel a cycle that
	// never had any Test items built at all as "Test completed by the deadline." -
	// confirmed live as a real false-positive on an autonomous cycle with a null Test.
	case len(c.Test) > 0 && c.IsComplete():
		c.Results = "Test completed by the deadline."
	default:
		c.Results = fmt.Sprintf("No results by the deadline of %s - that is itself the result.", c.Deadline.Format(time.RFC3339))
	}
	if err := s.update(ctx, thread, c); err != nil {
		return nil, err
	}
	return c, nil
}

// requireActiveLocked assumes the caller already holds mu - see activeLocked.
func (s *Store) requireActiveLocked(ctx context.Context, thread string) (*Cycle, error) {
	c, err := s.activeLocked(ctx, thread)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("no active cycle for thread %q", thread)
	}
	return c, nil
}

func encode(c *Cycle) ([]byte, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("encode cycle: %w", err)
	}
	return b, nil
}

// decode always trusts the caller's thread and id (the values that already located this exact
// entry) over whatever those fields happen to say inside the JSON payload - belt-and-suspenders:
// an entry corrupted by some other bug may have a payload disagreeing with the key that actually
// holds it, and every read of it should self-correct rather than propagate that lie forward.
func decode(id int64, thread string, data []byte) (*Cycle, error) {
	var c Cycle
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode cycle: %w", err)
	}
	c.ID = id
	c.Thread = thread
	return &c, nil
}
