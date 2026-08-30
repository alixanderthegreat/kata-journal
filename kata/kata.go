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
// the CLI's own default. Own bbolt file, pure Go with no cgo - nothing here needs SQL or
// embeddings, just "what's the active cycle for this thread."
package kata

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"go.etcd.io/bbolt"
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

// Store persists cycles in their own bbolt file - a single embedded, pure-Go key/value store,
// no SQL or embeddings needed for "what's the active cycle for this thread."
//
// Layout: one top-level bucket (cyclesBucket), holding one nested bucket per thread (see
// threadKey), holding one entry per cycle keyed by its id (see idKey, an 8-byte big-endian
// encoding so bbolt's lexicographic key order matches numeric id order). Kind, Thread, and every
// other Cycle field live inside the JSON value, same as the id itself - the key is purely for
// ordering and lookup, decode always trusts its own arguments over whatever the JSON payload
// claims (see decode's own doc comment). A second top-level bucket (metaBucket) holds
// project-wide settings that aren't scoped to any thread - currently just the root Challenge
// (see SetChallenge).
type Store struct {
	db *bbolt.DB

	// mu serializes every read-modify-write against this store. A Cycle read followed by a
	// write is not atomic on its own, so any caller sharing one Store across goroutines needs
	// this - coarse on purpose, this is a single-operator tool, not a multi-tenant service.
	mu sync.Mutex
}

// cyclesBucket is the top-level bbolt bucket holding every thread's cycles, each in its own
// nested bucket underneath it (see threadKey).
var cyclesBucket = []byte("cycles")

// metaBucket is the top-level bbolt bucket for settings scoped by project rather than by thread
// (see challengeKey) - a project's Challenge doesn't change cycle to cycle or thread to thread the
// way cycles do, so it lives one level above them, not nested under threadKey.
var metaBucket = []byte("meta")

// rootChallengeKey is the legacy single Challenge slot from before Challenge was scoped by
// project - every existing db predates that change, so Challenge still falls back to reading this
// key when a project's own challengeKey entry is unset, meaning an existing db's Challenge (e.g.
// this repo's own "Play.") keeps working with no migration step. SetChallenge never writes here
// again; only challengeKey is written going forward.
var rootChallengeKey = []byte("root_challenge")

// challengeKeyPrefix namespaces per-project Challenge entries within metaBucket - see challengeKey.
var challengeKeyPrefix = []byte("challenge:")

// challengeKey derives a project's own Challenge entry - project is the same repo/cwd identity
// used elsewhere (see the CLI's projectIdentity()), kept independent of thread so several threads
// within one project (e.g. one per agent) share the same Challenge rather than each getting their
// own fork of it.
func challengeKey(project string) []byte {
	return append(append([]byte{}, challengeKeyPrefix...), []byte(project)...)
}

// threadKey derives a nested bucket name from a thread - the db file itself is already a
// project's own scope (see Open), so this is purely the layer below that: several parallel
// lines of work sharing one file.
func threadKey(thread string) []byte {
	return []byte(thread)
}

// idKey encodes a cycle id as 8 bytes, big-endian - bbolt keys sort lexicographically as raw
// bytes, so a fixed-width big-endian encoding is what makes key order match numeric id order
// (a decimal string encoding would not: "10" sorts before "9").
func idKey(id int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(id))
	return b
}

func idFromKey(k []byte) int64 {
	return int64(binary.BigEndian.Uint64(k))
}

func Open(dbPath string) (*Store, error) {
	db, err := bbolt.Open(dbPath, 0o600, nil)
	if err != nil {
		return nil, fmt.Errorf("open bbolt: %w", err)
	}

	if err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(cyclesBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(metaBucket)
		return err
	}); err != nil {
		db.Close()
		return nil, fmt.Errorf("create top-level buckets: %w", err)
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// SetChallenge persists one project's Challenge into metaBucket, keyed by project (see
// challengeKey) - a one-time (or deliberately-changed) project-wide setting, not a per-cycle or
// per-thread argument. Overwrites whatever was there before for that project; a different
// project's Challenge, in the same db file or otherwise, is untouched.
func (s *Store) SetChallenge(ctx context.Context, project, challenge string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(metaBucket).Put(challengeKey(project), []byte(challenge))
	})
}

// Challenge returns one project's Challenge as set by SetChallenge, or "" if it's never been set
// for that project. Falls back to the legacy rootChallengeKey when the project's own entry is
// unset - see rootChallengeKey's own doc comment for why: every db that predates per-project
// Challenge scoping already has its one Challenge sitting there, and this makes it keep resolving
// with no migration step required.
func (s *Store) Challenge(ctx context.Context, project string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var challenge string
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(metaBucket)
		if v := bucket.Get(challengeKey(project)); len(v) > 0 {
			challenge = string(v)
			return nil
		}
		challenge = string(bucket.Get(rootChallengeKey))
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("query challenge: %w", err)
	}
	return challenge, nil
}

// threadBucket returns the nested bucket for thread, or nil if that thread has never had a
// cycle started - caller decides whether that's an error or just "nothing here yet."
func threadBucket(tx *bbolt.Tx, thread string) *bbolt.Bucket {
	cycles := tx.Bucket(cyclesBucket)
	if cycles == nil {
		return nil
	}
	return cycles.Bucket(threadKey(thread))
}

// threadBucketCreate is threadBucket but creates the nested bucket on first use - only ever
// called from inside an Update transaction (bbolt buckets can't be created from a View).
func threadBucketCreate(tx *bbolt.Tx, thread string) (*bbolt.Bucket, error) {
	cycles := tx.Bucket(cyclesBucket)
	if cycles == nil {
		return nil, fmt.Errorf("cycles bucket missing - Store not opened via Open")
	}
	return cycles.CreateBucketIfNotExists(threadKey(thread))
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
// moment the newest entry turns out to already be closed, there is no active cycle at all, so
// this can stop at the first entry instead of scanning the whole bucket.
func (s *Store) activeLocked(ctx context.Context, thread string) (*Cycle, error) {
	var result *Cycle
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, thread)
		if bucket == nil {
			return nil
		}
		k, v := bucket.Cursor().Last()
		if k == nil {
			return nil
		}
		c, err := decode(idFromKey(k), thread, v)
		if err != nil {
			return err
		}
		if c.ClosedAt == nil {
			result = c
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query active cycle: %w", err)
	}
	return result, nil
}

// LastClosed returns the most recently closed cycle for thread, or nil if none has ever closed -
// used to seed a new cycle's Challenge/Target from the previous one's retrospective ("beginning
// next kata now").
//
// Walks backward from the newest id: since Start refuses to open a new cycle while one is
// active, cycles within a thread start and close in strict id order, so the first closed cycle
// found walking backward (skipping only a possible still-open newest one) is the most recently
// closed - no need to compare ClosedAt timestamps across the whole bucket.
func (s *Store) LastClosed(ctx context.Context, thread string) (*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var result *Cycle
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, thread)
		if bucket == nil {
			return nil
		}
		cur := bucket.Cursor()
		for k, v := cur.Last(); k != nil; k, v = cur.Prev() {
			c, err := decode(idFromKey(k), thread, v)
			if err != nil {
				return err
			}
			if c.ClosedAt != nil {
				result = c
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query last closed cycle: %w", err)
	}
	return result, nil
}

// ClosedCycles returns every closed cycle for thread, newest first - the full history behind
// LastClosed's single most-recent one, for browsing back further than one cycle. Never a lot of
// rows for a single-operator tool, so a full bucket walk (same pattern as PruneAutonomous) is
// simpler than maintaining a separate index.
func (s *Store) ClosedCycles(ctx context.Context, thread string) ([]*Cycle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var results []*Cycle
	err := s.db.View(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, thread)
		if bucket == nil {
			return nil
		}
		cur := bucket.Cursor()
		for k, v := cur.Last(); k != nil; k, v = cur.Prev() {
			c, err := decode(idFromKey(k), thread, v)
			if err != nil {
				return err
			}
			if c.ClosedAt != nil {
				results = append(results, c)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("query closed cycles: %w", err)
	}
	return results, nil
}

// PruneAutonomous deletes every CLOSED autonomous cycle for thread beyond the most recent keep -
// ambient self-check cycling that piles up over time, not deliberately authored (directed) work.
// Directed cycles and the currently active one (ClosedAt == nil) are never touched, by
// construction: only entries that decode with Kind == "autonomous" AND a non-nil ClosedAt are
// ever candidates.
//
// A plain delete, not an archive, is the deliberate call: whatever mattered from one autonomous
// cycle is expected to already be carried forward into the next one's own Current Condition (via
// LastClosed's Results), so nothing this removes is otherwise-unrecoverable information - it's
// ambient noise that already compounded forward before being pruned.
func (s *Store) PruneAutonomous(ctx context.Context, thread string, keep int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := threadBucket(tx, thread)
		if bucket == nil {
			return nil
		}

		var toDelete [][]byte
		seenAutonomous := 0
		cur := bucket.Cursor()
		for k, v := cur.Last(); k != nil; k, v = cur.Prev() {
			c, err := decode(idFromKey(k), thread, v)
			if err != nil {
				return fmt.Errorf("decode cycle %d: %w", idFromKey(k), err)
			}
			if c.ClosedAt == nil || c.Kind != "autonomous" {
				continue
			}
			seenAutonomous++
			if seenAutonomous > keep {
				// Cursor-returned keys are only valid until the next cursor call or a bucket
				// mutation - copy before collecting for deletion after the scan completes.
				toDelete = append(toDelete, append([]byte(nil), k...))
			}
		}
		for _, k := range toDelete {
			if err := bucket.Delete(k); err != nil {
				return fmt.Errorf("delete pruned cycle %d: %w", idFromKey(k), err)
			}
		}
		return nil
	})
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
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := threadBucketCreate(tx, c.Thread)
		if err != nil {
			return fmt.Errorf("open thread bucket: %w", err)
		}
		k, _ := bucket.Cursor().Last()
		id := int64(0)
		if k != nil {
			id = idFromKey(k) + 1
		}
		c.ID = id
		data, err := encode(c)
		if err != nil {
			return err
		}
		return bucket.Put(idKey(id), data)
	})
}

// update scopes its write by thread+id, not id alone - the same numeric id legitimately exists
// across many different threads (each thread counts from its own zero, see insert), so this
// must never guess which thread a bare id belongs to.
//
// Takes thread as an explicit parameter rather than trusting c.Thread - every caller already has
// the real, trusted thread in hand (it's what located this exact cycle via activeLocked's own
// lookup a moment ago), so there's no reason to re-derive it from the Cycle's own decoded JSON,
// which for a row corrupted by some other bug may still disagree with the thread that actually
// holds it. Also overwrites c.Thread to match before encoding, so a legacy-corrupted entry
// self-heals the moment anything next mutates it, instead of staying wrong forever.
func (s *Store) update(ctx context.Context, thread string, c *Cycle) error {
	c.Thread = thread
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket, err := threadBucketCreate(tx, thread)
		if err != nil {
			return fmt.Errorf("open thread bucket: %w", err)
		}
		data, err := encode(c)
		if err != nil {
			return err
		}
		return bucket.Put(idKey(c.ID), data)
	})
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

// decode always trusts the caller's thread (the value that already located this exact entry via
// its thread bucket) over whatever Thread field happens to be embedded in its JSON payload -
// belt-and-suspenders alongside update()'s own thread parameter: an entry corrupted by some
// other bug may have a payload disagreeing with the thread that actually holds it, and every
// read of it should self-correct rather than propagate that lie forward.
func decode(id int64, thread string, data []byte) (*Cycle, error) {
	var c Cycle
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("decode cycle: %w", err)
	}
	c.ID = id
	c.Thread = thread
	return &c, nil
}
