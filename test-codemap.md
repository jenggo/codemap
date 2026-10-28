# Working a codebase

You are being evaluated on real development work, not on tool usage. You will do
a set of tasks in this repository the way a developer would, then write one
report about how it went.

The unit is a **task**, not a call. A task is done when you produce an answer a
reviewer can check against the source. A fluent, confident, wrong answer is the
worst outcome available to you, and it is worth more to the report than ten
awkward-but-correct ones.

---

## 0. How to run this

For each task:

1. **Write your answer first**, using codemap only — do not open files or grep
   the tree yet. If you cannot answer without falling back, do it, but note
   exactly what you were looking for when you gave up.
2. **Then verify every factual claim** you made, using anything you like: `go
   build`, `go list`, `go doc`, `git`, reading the file, grep.
3. **Grade honestly** and record it before moving on.

Step 1 before step 2 is the whole point. If you verify as you go, you are no
longer measuring what codemap alone gives you, and the report becomes worthless.
The fallback in step 1 is not a failure — it is the most useful thing you can
record.

Finish one task completely — answer, verification, grade — before starting the
next. Do not carry an answer forward, and do not let what you learned in task 3
soften your answer in task 4. If you notice that you already know something from
an earlier task, say so in that task's notes.

### Ground rules

- **Leave this repository as you found it.** Only T7 needs an edit; it says how
  to restore the tree, and `git status --porcelain` must be empty at the end.
  Everything else is read-only.
- **Every claim gets a verification command.** "I checked" is not a check. Quote
  the command or the `file:line` you looked at.
- **Do not soften a wrong answer into a right one.** If you said something false
  in step 1, record it as false. That is the finding.
- **Count your steps.** Tool calls, reads, searches, and elapsed time per task.

---

## 1. Choose the tasks

Run **6 to 10**. Not all fit every repo — a repo with no HTTP layer has no
request to trace. Skip a task if the construct genuinely does not exist, and say
why.

For each kind below, pick a concrete instance from this repo and write the
instantiated task down before you answer it:

- **Orient** — always runnable.
- **Impact** — an exported function with at least three callers, ideally one
  whose signature a developer would plausibly change.
- **Trace** — the main entry point, or the busiest handler. Pick one with a long
  real path and at least one similarly-named function that is *not* on it.
- **Locate** — a behaviour described in the README or a doc comment.
- **Plan** — the largest file by line count.
- **Verify** — an unexported function that looks unused, and one that looks used
  but is reachable only through an interface or reflection.
- **Delta** — your own edit, in a copy.
- **Cross-cutting** — an enum, command, or config field that is dispatched on in
  more than one place.

---

## 2. The tasks

Each block is the request as a user would phrase it — vague, goal-first, with
only the constraints they happened to remember. Answer the request, not this
document.

### T1 — Orient

```
I've just been dropped into this repo and I have to ship a fix by end of day.
Tell me what this codebase does, what the five most important packages are, and
where execution starts. I don't want a file listing, I want to understand it.
```

Correct means: what the module actually does, the packages that genuinely carry
the logic, and the real entry point. Not a paraphrase of the README, and not
"important" meaning "largest".

### T2 — Impact

```
I need to change the signature of <function>. Before I touch it, tell me
everything that would break — every caller, every implementation, every place
that assumes the current shape. I'd rather have a long list than miss one.
```

Correct means: every call site, including tests, plus anything that would stop
satisfying an interface. Count the truth first, then compare.

### T3 — Trace

```
Something calls <entry point> and I need to know what happens next, in order.
Walk me from the entry point to the database write. I need to know which
functions are actually on that path and which ones just look like they are.
```

Correct means: an ordered chain where every hop is a real call. Watch yourself
for a narrative that reads well but contains a hop you did not confirm, or that
picks up the decoy.

### T4 — Locate

```
Where is the code that handles <behaviour from the README>? Give me the file and
line, and tell me what else lives in that file so I know what I'm walking into.
```

Correct means: the implementation, not a caller, and an accurate account of what
else is in that file.

### T5 — Plan

```
<largest file> is too big and I want to split it. Give me a concrete plan:
which symbols move together, which ones can't be separated, and what order to do
it in so the build stays green the whole way.
```

Correct means: groupings that follow real coupling — shared unexported helpers,
mutual recursion, a type and its methods. Not grouping by name prefix.

### T6 — Verify, deletion (run this twice)

```
Is <unexported function> safe to delete? I think nothing calls it, but I want to
be sure before I remove it.
```

Run it once on genuinely dead code and once on code that is reachable only
through an interface or reflection. Correct means the truth in both cases, with
evidence. A confident "safe to delete" on the live one is the worst answer this
suite can produce — record it at the highest severity.

### T7 — Delta

```
I've been hacking on this for an hour. Tell me what I've actually changed and
what else I now need to update because of it.
```

This one needs a real edit. Confirm `git status --porcelain` is empty first — if
it is not, skip this task and say why. Then, in this repository:

1. Rename one exported function at its declaration and at one call site.
2. Append one new exported function to a file that already has symbols.
3. Create one new file with one function in it, and do not `git add` it.

Ask the question, then restore the tree exactly:

```bash
git checkout -- . && git clean -fd
git status --porcelain   # must be empty again
```

Correct means: the rename reported as a rename (old name and new name), the
addition as an addition, the untracked file counted, and the call sites that now
fail to compile listed.

If the tree is not clean after the restore, stop and put that at the top of the
report.

### T8 — Cross-cutting

```
We're adding <a new field / a new enum value / a new command>. Find every place
that would need to know about it — switches, maps, validators, serializers,
tests. I want a checklist I can work through.
```

Correct means: exhaustive, verified by searching for the name and for the
switches that dispatch on it. Not the first three hits.

### T9 — Cross-repo (only if this workspace has two or more modules)

```
The client and the server disagree about something and I can't find it. Compare
what <client module> sends with what <server module> expects, and tell me
exactly where they diverge.
```

Correct means: the specific field or message type that differs, named on both
sides, with `file:line`.

### T10 — Onboarding by example

```
I need to add <a new endpoint / a new subcommand / a new migration> that works
exactly like <an existing one>. Show me the existing one end to end and tell me
every file I'd have to touch to add a sibling.
```

Correct means: every file the existing instance touches, in the order a
developer would work through them — registration and tests included.

---

## 3. Grading

### Outcome

| | |
|---|---|
| `DONE` | correct and complete enough to act on |
| `PARTIAL` | correct as far as it goes, but a developer would still have to go and find something |
| `FAILED` | wrong, or too incomplete to help |
| `REFUSED` | you said you could not do it — record why, in your own words |

### Truth

Count three numbers per task: **claims made**, **claims wrong**, **claims you
could not verify**. Every one of the second kind is a finding, and its severity
follows from what a developer would do next, wrongly.

`DONE` with two wrong claims is not a success. Report it as it is.

### Effort

Record steps and elapsed time, then answer one sentence: **was that a reasonable
amount of work for a human with the same tools?** Ten searches for "where is the
retry logic" is normal. Ten searches to answer "what does this repo do" is not.

Also record **abandonment**: the moment you stopped relying on the index and fell
back to reading files or grepping the tree. Note what you were looking for. This
is the most useful line in the report — it marks exactly where the index stopped
being enough.

---

## 4. Severity

| Level | Meaning |
|---|---|
| HIGH | you acted on a wrong belief: called live code dead, missed a breaking caller, named something that does not exist |
| MED | the answer was right but incomplete enough to cost real work, or you had to abandon the index to finish |
| LOW | friction: extra steps, an answer that needed one more question to be usable |

---

## 5. The report

Write **one** file:

```
~/go/src/mcp/codemap/reviews/<repo>-<YYYY-MM-DD>.md
```

`<repo>` is this checkout's directory name. Create the `reviews` directory if it
is missing. If the path already exists, append `-2`, `-3`, and so on — never
overwrite an earlier report.

```markdown
# <repo> — agent tasks — <YYYY-MM-DD>

- repo: <absolute path>
- module: <module path>
- head: <sha>
- packages: <n>   go files: <n>
- tasks run: <n>

## Results

| # | kind | task | outcome | claims | wrong | steps | time |
|---|---|---|---|---|---|---|---|
| T1 | orient | what is this repo | DONE | 12 | 0 | 6 | 40s |
| T2 | impact | change <fn> signature | PARTIAL | 9 | 2 | 11 | 3m |

## Findings

### <task> — <one line>  [HIGH|MED|LOW]

- asked: <the verbatim prompt>
- I said: <quoted>
- truth: <what the source says>
- verify: `<command or file:line>`
- consequence: <what a developer would do next, wrongly>

## Where the index stopped being enough

- <task>: fell back to <what> while looking for <what> — <n> steps lost

## Effort

- median steps per task: <n>
- tasks finished without reading any source: <n>/<n>
- slowest task: <which>, <n> steps, <why>

## What would have made these tasks easier

Ranked by how many tasks it would have unblocked. One line each. Describe the
question you needed answered and could not ask — not a tool.
```

---

## 6. Before you finish

Check these, in this order:

1. **Did you run T6 twice?** The deletion answer is the highest-signal result in
   the report and it only exists if you instantiate both cases.
2. **Is every finding's `verify:` line a real command or `file:line`?** If not,
   the finding is a guess — mark it unverified or delete it.
3. **Did you modify this repository?** `git status --porcelain` must be clean.
   If it is not, revert it and say so in the report.
4. **Does the report name the single most valuable change**, with the task IDs
   that justify it? One sentence at the end.
