# kata

A standalone CLI for structured, iterative planning - a digital form of the [*Kata
Journal*](https://www.amazon.com/Kata-Journal-overcome-obstacles-achieve/dp/B0BM3SWP3J) method:
Challenge, Target Condition, Current Condition, Obstacles, Test, Expectations, Results.

No server, no accounts, no network calls. Each invocation opens a local file, does one thing, and
closes.

## What this is

The *Kata Journal* is a paper form for working through a challenge iteratively: state the why,
pick a small target, describe where you actually stand, name what's in the way, decide on one
concrete next move, say what you expect from it (including how long it should take), then do it
and record what actually happened. Then you do it again, informed by what you just learned.

This tool is that same seven-step form, backed by a small embedded database instead of a page in
a book. That difference matters for one part of the original form in particular: the book's
closing "Deep Reflection" section - what did you learn, what challenge lies ahead, what's next -
exists because a physical journal has to sum itself up somewhere before you turn the page. A
database doesn't have that constraint. Every cycle's `Results` is the reflection, and the next
cycle can read the previous one back (`kata history`) instead of needing a separate, dedicated
"final" section - the record just keeps compounding forward, cycle to cycle, without ever needing
to force closure.

## Install

Requires Go 1.26.5+ (see `go.mod`).

```sh
git clone git@github.com:alixanderthegreat/kata-journal.git
cd kata-journal
go build -o bin/kata .
```

Then either add `bin/` to your `PATH`, or invoke it as `./bin/kata` from the repo, or copy the
binary wherever you keep tools you run from anywhere.

## The form, in order

1. **Challenge** - the why. Set once per project, not something you retype every cycle.
2. **Target Condition** - the goal for this cycle, phrased as an end-state.
3. **Current Condition** - a plain, honest description of where things actually stand.
4. **Obstacles** - a flat, unordered list of what's in the way. Context, not a checklist -
   nothing here is required to pair up with a Test item.
5. **Test** - the concrete next move(s), constructed to advance Current Condition toward Target
   Condition. Informed by the Obstacles, but not indexed to them: a cycle can run a ten-item Test
   around zero named obstacles, or one obstacle and a Test that never directly addresses it.
6. **Expectations** - what the Test as a *whole* is expected to achieve, plus (optionally) a time
   estimate for how long that should take.
7. **Results** - the retrospective, recorded when the cycle closes. Whether the Test finished or
   the deadline simply arrived first, something honest gets written here - "no results by the
   deadline" is itself a valid result, not an error state.

Coming back to a project you haven't touched in a while? Run `kata orient` first - it prints
usage, the current active cycle (if any), and the most recently closed cycle (if any), in that
order, and never errors just because either one is empty. That's the "what does this do, am I
mid-cycle, what did I just decide" habit as one deterministic command instead of three remembered
ones.

## Walkthrough

```sh
$ kata challenge "get sharper"
ok

$ kata target "faster, shorter replies"
{
  "id": 0,
  "kind": "directed",
  "thread": "default",
  "challenge": "get sharper",
  "target_condition": "faster, shorter replies",
  ...
}

$ kata condition "sluggish, untuned"
ok

$ kata obstacle "no baseline latency numbers yet"
ok

$ kata test "tune sampling params"
ok

$ kata expectations "measurably faster replies by the end of this cycle" 30
ok

$ kata active
{
  "id": 0,
  "kind": "directed",
  "thread": "default",
  "challenge": "get sharper",
  "target_condition": "faster, shorter replies",
  "current_condition": "sluggish, untuned",
  "obstacles": [
    { "text": "no baseline latency numbers yet", "recorded_at": "..." }
  ],
  "test": [
    { "id": 0, "text": "tune sampling params", "done": false, "created_at": "..." }
  ],
  "expectations": "measurably faster replies by the end of this cycle",
  "results": "",
  "deadline": "...",
  "started_at": "..."
}

$ kata complete 0 "sampling params tuned, replies noticeably shorter"
ok

$ kata close "faster and shorter, momentum kept"
{ ..., "results": "faster and shorter, momentum kept", "closed_at": "..." }
```

If the deadline arrives before the Test is finished, `close` will refuse (it requires every Test
item complete) - use `kata close-expired [results]` instead, which closes honestly either way: an
incomplete Test past its deadline is not an error, it's just the cycle's actual outcome.

## Every verb

Run `kata help` for the full, current reference - it's kept accurate in the binary itself rather
than duplicated here. Short version:

| Verb | Does |
|---|---|
| `challenge <text>` | Set the project's Challenge - once, not per cycle |
| `target <target_condition>` | Open a cycle and set its Target Condition in one step |
| `condition <text>` | Set Current Condition |
| `obstacle <text>` | Append one Obstacle |
| `edit-obstacle <index> <text>` | Rewrite an Obstacle's text by its position; stamps `edited_at` |
| `test <text>` | Append one Test item |
| `edit-test <item_id> <text>` | Rewrite a Test item's text; refused once that item is complete |
| `expectations <text> [deadline_minutes]` | Set Expectations; optionally set/replace the Deadline |
| `complete <item_id> [results]` | Mark a Test item done |
| `close <results>` | Close the cycle - requires every Test item complete |
| `close-expired [results]` | Close honestly once the deadline has passed, complete or not |
| `active` | Show the current open cycle |
| `history` | Show the most recently closed cycle |
| `orient` | Re-orientation as one command: usage + active + history, in order; never errors on an empty active/history state |
| `log [keyword]` | List every closed cycle, newest first; `keyword` filters by substring across Challenge/Target/Current/Expectations/Results |

## Storage and scope

Cycles persist in a local [bbolt](https://github.com/etcd-io/bbolt) file - pure Go, no cgo, no
server process. Defaults to `./.kata/kata.db`, the same "each project gets its own store"
convention as `.git` itself; override with `KATA_DB_PATH`.

The db file is already a project's own scope. Underneath that, cycles are further scoped by a
single *thread* - most projects only ever need the default one, but `KATA_THREAD` lets several
parallel lines of work (e.g. one per agent or workstream) share the same database file without
stepping on each other's active cycle.

## Design notes

- **Obstacles are not a checklist.** They're a flat, unordered causality map - context for why
  things are hard, not action items that each need their own matching Test entry.
- **A missed deadline is a result, not a failure.** `close-expired` records honestly what did or
  didn't happen; it never blocks or errors just because the Test wasn't finished in time.
- **Challenge lives in the database, not an environment variable.** It's a project-wide constant
  (`kata challenge <text>`), so it survives across shells and machines instead of needing to be
  re-exported every session.
- **No semantic search, on purpose.** `kata log` filters by plain substring, not embeddings - a
  personal-scale journal doesn't have the volume to justify it, and it's one less dependency.
- **Edits are audited, not silent.** `edit-obstacle`/`edit-test` stamp `edited_at` rather than
  quietly overwriting text, and `edit-test` refuses once an item is complete - its `results` is
  the record of what actually happened, not something a later edit should be able to rewrite.
