---
name: kata-practice
description: Use this skill whenever running, reviewing, or advising on a kata cycle with the kata-journal CLI (this repo's own tool) - covers Target vs Target Condition, how Test items compose a whole, Expectations as a real falsifiable prediction, why the deadline placeholder is intentional, and the CLI's real behavior/gotchas.
---

# Working a kata cycle well

This tool implements Mike Rother's *Toyota Kata* improvement pattern (see `docs/` in this repo
for the two source chapters). The CLI's own `kata orient` / `kata help` covers mechanics. This
skill covers the judgment calls that mechanics alone don't teach - each one below was a real
correction made live, not a hypothetical.

## Target vs Target Condition

**A target is an outcome (a count). A target condition is a description of an operating pattern -
a checkable state, not a number.** "20 people saw this" is a target. "A repeatable, checkable
practice exists for X" is a target condition. Setting a target condition should NOT include how
you'll get there - that's what Test items are for. If you catch yourself writing a countermeasure
into the target condition, stop and move it to Test. See
`docs/establishing_a_target_condition.txt` (ch. 5) for the full argument,
including why "I don't know how I'll achieve this yet" is a sign you set it correctly, not a
problem to fix before setting it.

## Obstacles are context, not a checklist

A flat, unordered causality list - awareness of what's in the way, NOT one-to-one with Test items.
A cycle can run a real Test with zero obstacles resolved, or resolve zero obstacles and still hit
the target condition. Don't try to pair them up.

## Test items compose a whole - not each a whole experiment

`Expectations` describes what the Test *as a whole* is meant to achieve. Individual test items are
pieces of that - orientation, a friction step (e.g. account setup), the actual action, and logging
can each be their own item; no single item needs to independently be a complete PDCA cycle. What
matters across the set: prefer a provisional step now, with whatever's already on hand, over a
polished one later; and prefer firsthand observation ("go and see") over secondhand opinion about
what might work - what Rother's own book calls "testing over talking." See
`docs/moving_toward_a_target_condition.txt` (ch. 6). The Five Questions
from that chapter are the fastest way to keep a stalled cycle moving: what's the target condition,
what's the actual condition now, what obstacle are you addressing right now (not all of them),
what's your next step, and when will you go check what you learned from taking it.

## Expectations should be a real, falsifiable prediction

Not just "what the Test achieves" in the abstract - state what you actually predict will happen,
optimistic or pessimistic, before the Test runs. An unstated prediction can't be confirmed or
refuted against Results at close; a stated one can, which is what makes the cycle an actual
experiment rather than just logged activity. A confirmed pessimistic prediction is real, valuable
data, not a failed effort - don't let a bad number push you toward reopening or redesigning
something the record hasn't actually tested yet.

## The deadline placeholder is intentional, not a bug

`kata target` sets a short (15 min) placeholder deadline on purpose - a forcing function against
opening a cycle and never coming back to actually define the rest of it. Don't rush to "fix" it
defensively the moment you notice it; if it lapses before the cycle's fully defined, that's real
information about whether the cycle was ever seriously being worked. Use
`kata expectations <text> <minutes>` to set a real deadline once real thought has gone into the
timeframe - not before.

## CLI behavior worth knowing before it surprises you

- `challenge` is set once for the whole project db, not per cycle - a fixed north star.
- `edit-obstacle` and `edit-test` stamp `edited_at` rather than silently overwriting; `edit-test`
  additionally refuses outright once that item is `Done`, because rewriting what was tested after
  the fact is exactly the revisionism this tool exists to prevent.
- `complete` on an already-`Done` item does NOT refuse and does NOT overwrite - it appends a new
  entry to that item's `ResultsHistory`. This was a real bug found live (an early correction
  silently clobbered the honest record of a first, worse attempt) and fixed to append instead -
  use this deliberately: a later `complete` call is how you add a correction or a follow-up log
  entry without losing what was there before.
- A text argument containing a bare `$` or a backtick can get shell-expanded before the CLI ever
  sees it - e.g. `kata condition "spent $32.10 on coffee"` silently records "spent 2.10 on coffee"
  (bash reads `$3` as an empty positional param). Quote carefully, or better: pass `-` in place of
  any `<text>`/`<results>`/`<target_condition>` argument to read it from stdin instead, and supply
  it via a quoted heredoc delimiter - the one form immune to expansion regardless of content:
  ```
  kata condition - <<'EOF'
  spent $32.10 on coffee
  EOF
  ```
  Prefer this stdin form over an inline double-quoted arg for any text that isn't known in advance
  to be free of `$`/backtick/other shell metacharacters - that covers most bot/agent-generated text.
- `kata show <field>` prints one field of the active cycle as plain text - no jq needed for the
  common "what's the target/obstacles/test right now" check. `show test` prints each item's
  `ResultsHistory` indented underneath it, not just the text and done/not-done mark - it missed
  that on the first pass and was fixed once noticed, since a status check that hides what actually
  happened isn't much of a status check.

## Don't sanitize the record

An imperfect result, a typo, a bad first pass, a correction - all of it is allowed to stay in the
permanent record rather than getting cleaned up after the fact. The journal being honest is more
valuable than it being tidy.
