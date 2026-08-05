// Command kata is a standalone CLI for the kata structured-planning tool. It talks directly to
// a local kata.Store (no HTTP server, no "is the server running?" step) - a CLI invocation opens
// the bbolt file, does one thing, and closes, which is safe precisely because nothing else
// needs to hold that file open concurrently (see kata.Store's own doc comment on why it's a
// coarse single-mutex store regardless).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/alixanderthegreat/kata-journal/kata"
)

// dbPath defaults to a dotdir under the current working directory - the same "each project gets
// its own scope" convention as .git itself, so running this from inside any project's own repo
// just works with zero configuration. Override via KATA_DB_PATH for a shared/central store.
func dbPath() string {
	if p := os.Getenv("KATA_DB_PATH"); p != "" {
		return p
	}
	return filepath.Join(".kata", "kata.db")
}

// thread defaults to "default" - most projects only ever want one kata thread at a time, and
// requiring an identity arg on every call would be pure friction for that common case. The db
// file itself is already a project's own scope (same convention as .git), so this is purely the
// layer below that: KATA_THREAD overrides it for a project that wants several parallel threads
// (e.g. one per agent/workstream) sharing the same database file.
func thread() string {
	if v := os.Getenv("KATA_THREAD"); v != "" {
		return v
	}
	return "default"
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(dbPath()), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "kata: create db directory:", err)
		os.Exit(1)
	}
	store, err := kata.Open(dbPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "kata: open store:", err)
		os.Exit(1)
	}
	defer store.Close()

	if err := run(store, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "kata:", err)
		os.Exit(1)
	}
}

func run(store *kata.Store, verb string, args []string) error {
	ctx := context.Background()
	thread := thread()

	switch verb {
	case "challenge":
		return cmdText(args, "challenge", func(text string) error {
			return store.SetChallenge(ctx, text)
		})
	case "target":
		return cmdTarget(ctx, store, thread, args)
	case "condition":
		return cmdText(args, "condition", func(text string) error {
			return store.SetCurrentCondition(ctx, thread, text)
		})
	case "obstacle":
		return cmdText(args, "obstacle", func(text string) error {
			return store.AddObstacle(ctx, thread, text)
		})
	case "test":
		return cmdText(args, "test", func(text string) error {
			return store.AddTestItem(ctx, thread, text)
		})
	case "expectations":
		return cmdExpectations(ctx, store, thread, args)
	case "complete":
		return cmdComplete(ctx, store, thread, args)
	case "close":
		return cmdClose(ctx, store, thread, args)
	case "close-expired":
		results := ""
		if len(args) > 0 {
			results = args[0]
		}
		c, err := store.CloseIfDeadlinePassed(ctx, thread, results)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("no active cycle for thread %q past its deadline", thread)
		}
		return printCycle(c)
	case "active":
		c, err := store.Active(ctx, thread)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("no active cycle for thread %q", thread)
		}
		return printCycle(c)
	case "history":
		c, err := store.LastClosed(ctx, thread)
		if err != nil {
			return err
		}
		if c == nil {
			return fmt.Errorf("no closed cycle yet for thread %q", thread)
		}
		return printCycle(c)
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown verb %q", verb)
	}
}

// cmdTarget opens a new cycle and records its Target Condition in one step - once a target is
// set, the cycle has started; there's no separate "start" action to take first. Current
// Condition (set afterward via `condition`) starts empty, and Deadline starts at a placeholder
// until `expectations` sets a real one.
func cmdTarget(ctx context.Context, store *kata.Store, thread string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kata target <target_condition>")
	}
	challenge, err := store.Challenge(ctx)
	if err != nil {
		return err
	}
	if challenge == "" {
		return fmt.Errorf("no Challenge set - Challenge is fixed project config, not a per-cycle argument; set it once with `kata challenge \"...\"` and re-run")
	}
	deadline := time.Now().Add(15 * time.Minute) // placeholder default until `expectations` sets a real one
	c, err := store.Start(ctx, thread, "directed", challenge, args[0], "", deadline)
	if err != nil {
		return err
	}
	return printCycle(c)
}

func cmdText(args []string, verb string, fn func(string) error) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kata %s <text>", verb)
	}
	if err := fn(args[0]); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

// cmdExpectations sets what the Test as a whole is expected to achieve, and optionally, in the
// same call, how long that's expected to take - a time estimate is itself part of the
// expectation, not a separate unrelated setting.
func cmdExpectations(ctx context.Context, store *kata.Store, thread string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kata expectations <text> [deadline_minutes]")
	}
	if err := store.SetExpectations(ctx, thread, args[0]); err != nil {
		return err
	}
	if len(args) > 1 {
		minutes, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("deadline_minutes must be an integer: %w", err)
		}
		if err := store.SetDeadline(ctx, thread, time.Now().Add(time.Duration(minutes)*time.Minute)); err != nil {
			return err
		}
	}
	fmt.Println("ok")
	return nil
}

func cmdComplete(ctx context.Context, store *kata.Store, thread string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kata complete <item_id> [results]")
	}
	id, err := strconv.Atoi(args[0])
	if err != nil {
		return fmt.Errorf("item_id must be an integer: %w", err)
	}
	results := ""
	if len(args) > 1 {
		results = args[1]
	}
	if err := store.CompleteItem(ctx, thread, id, results); err != nil {
		return err
	}
	fmt.Println("ok")
	return nil
}

func cmdClose(ctx context.Context, store *kata.Store, thread string, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: kata close <results>")
	}
	c, err := store.CloseCycle(ctx, thread, args[0])
	if err != nil {
		if strings.Contains(err.Error(), "incomplete test items") {
			fmt.Fprintln(os.Stderr, "hint: close requires every Test item complete - finish the "+
				"remaining ones with `kata complete <item_id>`, or use `kata close-expired` instead "+
				"if the deadline has already passed.")
		}
		return err
	}
	return printCycle(c)
}

func printCycle(c *kata.Cycle) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func usage() {
	fmt.Print(`kata - a standalone structured-planning tool

Usage: kata <verb> [args...]

Storage: a bbolt file at ./.kata/kata.db by default (override with KATA_DB_PATH) - no server to
start or stop, each invocation opens the file, does one thing, and closes.

Scope: a single fixed thread by default (override with KATA_THREAD for several parallel threads
sharing one database file) - the db file itself is already a project's own scope.

Fields, in the canonical form:
  Challenge          the why. Set once with 'kata challenge <text>', persisted into the project's
                     own db - a fixed north star, not a per-cycle variable you retype each time.
                     Required before 'target' will work.
  Target Condition   the goal, phrased as an end-state. Setting it is what opens a cycle - there
                     is no separate "start" step.
  Current Condition  where things stand right now - diagnostic detail/evidence belongs here,
                     not in Challenge.
  Obstacles          a flat, unordered causality list - NOT a checklist paired 1:1 with Test
                     items. Context/awareness, not action items to individually resolve.
  Test               items constructed holistically to move Current toward Target, informed
                     by Obstacles but not indexed to them.
  Expectations       what the Test as a whole achieves - one cycle-level field, not per-item.
                     A time estimate (Deadline) can be set alongside it.
  Results            the retrospective, recorded on close.
  Deadline           closes the cycle regardless of Test completion - no results by the
                     deadline is itself a result, not an error state.

Verbs:
  challenge <text>               set the project's Challenge (see above) - once, not per cycle
  target <target_condition>      open a cycle and set its Target Condition in one step
  condition <text>              set Current Condition
  obstacle <text>               append one Obstacle - context, not paired 1:1 with Test items
  test <text>                   append one Test item - a move toward Target, not a response to
                                 any one Obstacle
  expectations <text> [deadline_minutes]
                                 deadline_minutes replaces the placeholder set by 'target'
  complete <item_id> [results]
  close <results>                requires every Test item complete (see close-expired otherwise)
  close-expired [results]        honest close once the deadline has actually passed, even with
                                 incomplete Test items; supply results if you have them, otherwise
                                 gets an honest "no results by the deadline" default
  active                        show the current open cycle
  history                       show the most recently closed cycle

Caution: a text argument containing a bare $ or a backtick can get expanded or executed by your
own shell before this program ever sees it (e.g. inside a double-quoted bash argument) - the
damage happens at your shell's parse time, this program has no way to catch it after the fact.
Quote carefully, or the permanent record ends up holding something other than what you meant.
`)
}
