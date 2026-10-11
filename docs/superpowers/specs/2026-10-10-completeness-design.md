# Taskflow Completeness Enforcement — Design Specification

**Date:** 2026-10-10
**Status:** Draft
**Author:** Brandon Lamer-Connolly + Claude
**Builds on:** [2026-07-19-taskflow-design.md](2026-07-19-taskflow-design.md)

---

## 1. Overview

A feature is not complete when its goal is reached. It is complete when **every relevant part of the project is in sync with it**. A new React screen is done only when the screen meets its requirements *and* its unit, integration, regression, contract, and e2e tests are updated *and* Storybook stories exist for the screen and its components.

This spec adds a **completeness layer** to taskflow. It works with the agent-context files that b1CodingTool generates (`b1 pair` → `AGENTS.md`):

- **b1CodingTool defines what "done" means.** Modules carry the rules for a stack, and `b1 pair` renders them in two forms: prose in `AGENTS.md` that the agent reads, and a machine-readable manifest that taskflow reads.
- **taskflow checks it during the session.** At every checkpoint it compares what changed in the project against the manifest, keeps a list of *obligations* (things still owed), reminds the agent about missing work in tool responses, and applies a completion gate at stop.

### Guiding Principle: Completeness Serves the Update Stream

Taskflow's main purpose does not change: **ClickUp tasks get frequent updates with decisions, notes, and work done, not just a final summary and a status change.** Completeness is designed so it *produces* those updates instead of competing with them:

| Completeness event | ClickUp effect |
|---|---|
| Obligations discovered or satisfied | ClickUp checklist on the task updates live |
| Agent waives an obligation | Posted as a 💡 Decision comment with the rationale (via the existing `LogDecision`) |
| Checkpoint | Existing progress comment gains a one-line completeness summary |
| Stop | "Session Finished" comment becomes a real completeness report, not just counts |
| Stop with required work outstanding | Task moves to a review status (e.g. "in review") instead of complete |

### Why Two Systems

| | `AGENTS.md` (b1CodingTool) | taskflow |
|---|---|---|
| When it acts | Before work starts, read once | At every checkpoint and at stop |
| Specificity | General ("screens need stories") | Specific ("`CheckoutPage.stories.tsx` is missing") |
| Can verify? | No | Yes: files, contents, commands, attestations |
| Leaves a record? | No | Yes: obligation table and ClickUp |
| Weakness | Gets diluted as context fills; can't check anything | Doesn't know the rules on its own |

Each covers the other's weakness. Tool responses matter most here: whatever `tf_checkpoint` returns lands in the agent's context right when it's relevant, which is far stronger than a rule the agent read many turns earlier.

### Key Design Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Where rules are written | b1 modules (+ project overrides) | One source; rendered as prose for agents and a manifest for taskflow |
| How taskflow gets the rules | Reads a compiled manifest file in the project | Taskflow depends on a file format, not on b1; any tool can produce the file |
| Manifest format | YAML | Matches b1's existing `b1-module.yaml` |
| What gets checked | Diff from the session's starting commit (`base_sha`) + untracked files | Catches work that's already committed, not just uncommitted changes |
| Verification | Three labeled levels: file checks, commands, agent attestation | Honest about what can and can't be proven |
| Skipping an obligation | Requires a waiver rationale, recorded as a Decision | Turns a skipped requirement into the update stream's best content |
| Default gate | `review` (complete locally, ClickUp → review status) | Visible without blocking until the rules have proven themselves |
| ClickUp checklist sync | Reconcile the full desired state each time, idempotently | Avoids storing ClickUp-returned IDs; safe to retry |
| Opt-in | No manifest = no behavior change | Projects without b1 or rules are unaffected |
| Up-front planning | Suggested, not required (`suggested_plan` + reminders) | Keyword matching is too unreliable to block on; early plans put the checklist in ClickUp before code exists |
| ClickUp status mapping | Lifecycle roles with type-based defaults + optional name overrides | Fixes the existing `active`/`closed` mapping bug (§8) without breaking the setup-free flow |
| Restarting a completed task | Unresolved obligations and waivers are inherited by the new session | Rework doesn't lose what was still owed, and earlier decisions aren't asked again |

### Non-Goals

- **Proving that tests cover what they should.** Taskflow checks that the expected artifacts exist and changed; it doesn't judge test quality. Attestations plus ClickUp visibility leave that to a human.
- **Running full test suites on every checkpoint.** Command checks are opt-in and run at stop or on demand.
- **Editing `AGENTS.md`.** That's b1's job. Taskflow only *suggests* changes (§9).
- **LLM-based judging.** Possible later; out of scope.

---

## 2. Concepts

| Term | Meaning |
|---|---|
| **Manifest** | `.agents/completeness.yaml` in the project: the compiled set of rules |
| **Rule** | "When a file matching X changes, these requirements apply" |
| **Requirement** | One thing a rule demands (a story file, an e2e change, a contract test) |
| **Subject** | The concrete file that triggered a rule (e.g. `src/features/payments/pages/CheckoutPage.tsx`) |
| **Obligation** | One requirement applied to one subject in one session; a row in `obligations` |
| **Verify level** | How an obligation is checked: `structural`, `command`, or `attest` |
| **Waiver** | An agent- or human-provided rationale for skipping an obligation; stored as a Decision |
| **Gate** | What `Stop(completed)` does while required obligations are open: `report`, `review`, or `block` |

---

## 3. The Completeness Manifest

### Location and Loading

Taskflow looks for the manifest under the session's `project_path`, in order:

1. `.agents/completeness.yaml` (written by `b1 pair`)
2. `.taskflow/completeness.yaml` (hand-written, for projects without b1)

The manifest is re-read on every evaluation, with no caching, so edits take effect immediately. If neither file exists, completeness is turned off for that session and every code path behaves exactly as it does today.

New dependency: `gopkg.in/yaml.v3`.

### Schema

```yaml
version: 1
gate: review                 # optional; can raise the global config's gate, never lower it (report | review | block)
sources:                     # informational; which b1 modules contributed (used by the drift report)
  - framework/react-web@1.0.0
  - testing/storybook@1.0.0
  - testing/playwright@1.0.0

stacks:                      # one per framework in the repo; see "Stacks and Command Resolution" below
  - id: web
    source: framework/react-web
    root: "."                  # e.g. "frontend" in a monorepo
    env: { CI: "true" }        # keeps Vitest/Jest out of watch mode
    vars:
      pm:                      # package manager, first match wins
        - { if: { exists: "pnpm-lock.yaml" }, value: "pnpm" }
        - { if: { exists: "yarn.lock" }, value: "yarn" }
        - { if: { any: [{ exists: "bun.lock" }, { exists: "bun.lockb" }] }, value: "bun" }
        - { value: "npm" }
    commands:
      test:
        - { if: { has_script: "test:ci" }, run: "{pm} run test:ci" }
        - { if: { has_script: "test" }, run: "{pm} test" }
      storybook-test:          # contributed by testing/storybook
        - { if: { has_script: "test-storybook" }, run: "{pm} run test-storybook" }

rules:
  - id: react-screen
    source: framework/react-web
    stack: web                 # paths below are relative to the stack's root
    describe: "New screen"
    when:
      added: "src/features/{feature}/pages/{name}.tsx"
      exclude: ["**/*.stories.tsx", "**/*.test.tsx"]
    plannable: true            # can be declared up front with tf_plan (§6)
    plan_hints: ["screen", "page", "view"]   # task-description keywords that trigger a suggested_plan (§7)
    require:
      - id: story
        source: testing/storybook
        severity: required
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.stories.tsx"
          contains: ["export default", "{name}"]

      - id: unit-test
        severity: required
        describe: "Component test for {name}"
        verify:
          exists: "src/features/{feature}/pages/{name}.test.tsx"

      - id: e2e
        source: testing/playwright
        severity: required
        describe: "E2E coverage for the {name} route"
        verify:
          changed: "e2e/**/*.spec.ts"
          contains_any: ["{name}", "{feature}"]
        attest: "Name the spec file and test cases that cover {name}"

      - id: visual-regression
        source: testing/storybook
        severity: recommended
        describe: "Visual/interaction regression for {name} via its stories"
        verify:
          contains_in: { file: "src/features/{feature}/pages/{name}.stories.tsx", any: ["play:", "parameters:"] }
        run: { command: storybook-test }   # Phase 4; until then the file check alone applies

      - id: contract
        severity: required
        describe: "Contract tests for changed {feature} services"
        only_if:
          changed: "src/features/{feature}/services/*.ts"
        verify:
          changed: "tests/contract/**"
        run: { command: test, args: ["tests/contract"] }   # optional command check (§4)

  - id: react-component
    source: testing/storybook
    stack: web
    describe: "New shared component"
    when:
      added: "src/core/components/{name}/{name}.tsx"
    require:
      - id: story
        severity: required
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/core/components/{name}/{name}.stories.tsx"
          min_matches: { pattern: "export const \\w+", count: 2 }

  # Every new child component of a screen needs its own stories (§17, decision 3)
  - id: react-feature-component
    source: testing/storybook
    stack: web
    describe: "New feature component"
    when:
      added: "src/features/{feature}/components/{name}.tsx"
      exclude: ["**/index.ts", "**/*.stories.tsx", "**/*.test.tsx", "**/use*.ts", "**/use*.tsx"]
    require:
      - id: story
        severity: required
        describe: "Storybook stories for {name}"
        verify:
          exists: "src/features/{feature}/components/{name}.stories.tsx"
          contains: ["export default", "{name}"]

  # Regression = the existing suite still passes. One per stack, not per file (§17, decisions 2 and 7).
  # b1 emits this rule once for every stack in the manifest.
  - id: regression-suite
    source: framework/react-web
    stack: web
    describe: "Existing {stack} test suite still passes"
    when:
      changed: "**"            # any change under the stack root makes the suite relevant
    scope: stack               # one obligation per stack per session
    require:
      - id: suite
        severity: required
        describe: "Full {stack} test suite passes"
        run: { command: test }
        attest: "Run `{command}` in `{root}` and paste the final summary line"
        attest_when: no_command
```

### Pattern Syntax

- `{var}`: captures one path-segment fragment (no `/`). Captured values can be used in every later template within the same rule.
- `*`: matches within one segment; `**`: matches any number of segments.
- Built-in variables: `{path}` (subject path), `{dir}` (subject directory), `{stem}` (file name without extension).

### Trigger Kinds (`when`)

| Key | Matches files that are… |
|---|---|
| `added` | new compared to `base_sha` (including untracked) |
| `modified` | present at `base_sha` and changed |
| `changed` | added or modified |
| `deleted` | removed (reserved for Phase 4: "deleted screen → delete its stories") |

`exclude` takes a list of globs that suppress the trigger.

### Rule Fields

| Field | Meaning |
|---|---|
| `id`, `describe`, `source` | Identity, human text, contributing b1 module |
| `stack` | Optional stack id. `when`/`verify` paths are relative to that stack's `root`, and `run: { command }` refers to its commands. Without `stack`, paths are relative to the project root |
| `when` | Trigger (above) |
| `scope` | `file` (default): one obligation per matching file. `stack`: one obligation per stack per session once anything matches; the subject is the stack id, or `*` for a rule with no stack (used for suite-wide checks such as regression) |
| `plannable` | Can be declared up front via `tf_plan` |
| `plan_hints` | Case-insensitive keywords; if any appears in the ClickUp task name or description and no plan exists, the start contract carries a `suggested_plan` (§7). ClickUp tags are deliberately not read (§17, decision 6) |

### Requirement Fields

| Field | Meaning |
|---|---|
| `id` | Stable within its rule; obligation identity depends on it |
| `severity` | `required` (affects the gate) or `recommended` (reported only) |
| `describe` | Human text, templated; used in tool responses, `AGENTS.md`, and the ClickUp checklist |
| `only_if` | Precondition on the changed file set; if it's false, no obligation is created |
| `verify` | File checks (§4): `exists`, `changed`, `contains`, `contains_any`, `contains_in` (check a file other than the one in `exists`), `min_matches`, `any_of`, `all_of` |
| `attest` | Prompt text; if present, the obligation also needs an attestation |
| `attest_when` | `always` (default) or `no_command`: attestation is required only when `run` can't execute (command checks disabled, or no command could be resolved for the stack), so it stands in for the command check |
| `run` | Optional command check (§4). Either `{ command: <name>, args: [...] }`, referring to the stack's resolved command (args are shell-quoted and appended), or a literal string (for project overrides) |

Templates in `describe` and `attest` can also use `{stack}`, `{root}`, and `{command}` (the resolved command line, or "the project's test suite" if none was resolved).

**Satisfaction rule:** an obligation is satisfied when (`verify` passes, or there is no `verify`) **and** (an attestation has been provided, or no attestation is required under `attest_when`) **and** (`run` passed, or `run` isn't enabled, isn't present, or didn't resolve to a command).

### Stacks and Command Resolution

Your projects span React (web and native/Expo), Flutter, and SwiftUI frontends and Django, django-ninja, and FastAPI backends, often in the same repo. So test commands are **never hardcoded in rules**. Each stack declares *candidate* commands with conditions, and taskflow picks the first one whose condition holds, on every evaluation.

**Who knows what:**
- **b1 framework modules** own the knowledge of how each framework is tested (which lockfiles, scripts, and config files matter). They ship a `stack` block in their `completeness.yaml`.
- **`b1 pair`** works out which stacks exist and where (by marker file: `package.json`, `pubspec.yaml`, `Package.swift`/`*.xcodeproj`, `manage.py`, `pyproject.toml`) and writes them into the manifest with their `root`.
- **taskflow** evaluates the conditions in the project at runtime. It has no framework-specific code; it only knows the condition vocabulary below. Because resolution happens at runtime, switching package managers or adding a `test:ci` script takes effect without re-running `b1 pair`.

**Condition vocabulary** (all paths relative to the stack root):

| Condition | True when |
|---|---|
| `exists: <glob>` | A matching file or directory exists |
| `has_script: <name>` | `package.json` has `scripts.<name>` |
| `file_contains: { file, text }` | The file exists and contains the text |
| `any: [...]` / `all: [...]` | Combinations |
| *(no `if`)* | Always; used as the final fallback |

`vars` resolve the same way (first match wins) and can be used in commands as `{name}`. Project overrides in `.agents/project/completeness.yaml` can set vars or pin a command outright, skipping detection:

```yaml
stacks:
  ios:
    vars: { scheme: "MyApp", destination: "platform=iOS Simulator,name=iPhone 16" }
  api:
    commands: { test: "uv run pytest -x" }
```

**Default detection each framework module ships:**

| Module | Detection | `test` command |
|---|---|---|
| `react-web` | `{pm}` from lockfile (pnpm → yarn → bun → npm); `CI=true` | `test:ci` script, else `test` script |
| `react-native` (Expo) | Same as `react-web` | Same (typically `jest-expo`) |
| `flutter` | `pubspec.yaml` | `flutter test` (`integration_test/` stays with the e2e requirements) |
| `swiftui` | `Package.swift` → SwiftPM; else Xcode project | `swift test`; else `xcodebuild test -scheme {scheme} -destination '{destination}'` (needs both vars from the project override, otherwise unresolved) |
| `django`, `django-ninja` | `{py}`: `uv.lock` → `uv run`, `poetry.lock` → `poetry run`, else `.venv/bin/` prefix | `{py} pytest` if pytest is configured (`pytest.ini`, `conftest.py`, or `[tool.pytest` in `pyproject.toml`), else `{py} python manage.py test` |
| `fastapi` | Same `{py}` detection | `{py} pytest` |

**When nothing resolves** (e.g. a SwiftUI app without `scheme`/`destination`): the obligation isn't failed. It falls back to attestation (`attest_when: no_command`), and the report carries a warning naming the stack and the override that would fix it. Resolved commands are shown in the start contract and in attest prompts from Phase 1, so the agent knows exactly what to run even before taskflow can run it (Phase 4).

---

## 4. Verification Levels

| Level | Mechanism | When it runs | Confidence |
|---|---|---|---|
| **Structural** | `exists` / `changed` / `contains` / `min_matches` against the filesystem and changed set | Every evaluation (start, checkpoint, `tf_completeness`, stop) | Deterministic; can be gamed with stub files |
| **Command** | `run:` command executed in `project_path`; exit code 0 = pass | Stop, or on demand via `tf_verify` | High; cost depends on the project |
| **Attest** | Agent submits evidence via `tf_attest` | Whenever the agent calls it | Claimed only; made visible in ClickUp for human review |

Every obligation records which levels it passed, and the ClickUp checklist and stop report show them ("e2e — attested, structural ✓"). A reviewer can tell a verified item from a claimed one at a glance.

**Command safety:** commands come *only* from the manifest and project overrides, never from tool arguments. Detection only chooses among candidates the manifest declares, and `{var}` values come from detectors or overrides. Command checks are off unless `completeness.allow_commands = true` in `~/.taskflow/config.toml`. Each command runs in its stack's `root`, with the stack's `env` merged over the inherited environment and a time limit of `completeness.command_timeout_s` (default 300). Output is truncated before storage.

**Gaming:** an agent can satisfy a file check with a stub. `contains` / `min_matches` raise the bar, command checks raise it further, and listing the evidence in ClickUp exposes what's left. This layer is a strong nudge plus an audit trail, not a proof system.

---

## 5. Evaluation

### Starting Commit (`base_sha`)

`Session` gets a `base_sha`, recorded **when the session is first created** in `Service.Start`. Resuming a `PAUSED` session must not overwrite it. In a repo with no commits, `base_sha` is the empty tree (`4b825dc642cb6eb9a060e54bf8d69288fbee4904`).

### Inheritance on Restart

When `Start` creates a new session for a task whose previous session is `COMPLETED` or `ARCHIVED`, it copies that session's `open`, `attested`, and `waived` obligations into the new session with `inherited_from` set to the source obligation's ID:

- Copies keep their status, evidence, and `waiver_decision_id`, so earlier decisions aren't asked again. The agent can re-open a waiver by attesting or satisfying the obligation.
- `satisfied` and `void` obligations aren't copied. If the rework breaks one, the diff will trigger it again.
- Inherited obligations are evaluated like planned ones: their subject is checked directly, whether or not it appears in the new diff.
- The start contract lists them under `completeness.inherited` so the agent knows what it's taking on.

### Changed Set

`internal/gitctx` gains:

```go
type ChangeType string // "A", "M", "D", "R"

type ChangedFile struct {
    Path    string
    OldPath string // renames only
    Type    ChangeType
}

func ChangedSince(projectPath, baseSHA string) ([]ChangedFile, error)
```

Implementation: `git diff --name-status -z -M <base_sha>` (working tree compared to base, which includes both committed and uncommitted work), combined with untracked files from `git status --porcelain=v1 -z` (`??` → `A`). `.b1codes/` paths are excluded, as `Capture` does today, and the result keeps the existing 500-file cap.

If `base_sha` can't be reached (rebased away, or the branch was force-reset), fall back to `git merge-base HEAD <default_branch>` (from `.agents/config.yaml`'s `default_branch`, or `main`). If that also fails, evaluate dirty files only and add a warning to the result.

### Algorithm

```
evaluate(session):
  manifest ← load(project_path)            ; nil → return nil (feature off)
  changed  ← gitctx.ChangedSince(base_sha)
  found    ← {}
  for rule in manifest.rules:
    for file in changed matching rule.when (minus exclude):
      vars ← captures(rule.when, file) ∪ builtins(file)
      for req in rule.require:
        if req.only_if and not match(req.only_if, changed, vars): continue
        found[(rule.id, req.id, file)] ← check(req, vars, changed)
  for planned or inherited obligation in store:
    found[...] ← check(...)                ; subject may not exist yet
  merge(found, store.ListObligations(session)):
    - new key           → insert (status open | satisfied), first_seen_at = now
    - existing, waived  → unchanged (sticky)
    - existing, other   → recompute open ⇄ satisfied; set satisfied_at on transition
    - stored key not in found and not planned/inherited → status void (e.g. file reverted)
  return CompletenessReport{summary, open, newly_open, newly_satisfied, warnings}
```

Evaluation is idempotent and cheap (one `git diff`, a few `stat`s and file reads), so it runs on every checkpoint.

### Where It Runs

| Call | Evaluates? | Returned to agent |
|---|---|---|
| `Start` | Yes (after git capture) | `contract.completeness` (§7) |
| `Checkpoint` | Yes | Completeness changes since the last evaluation |
| `tf_completeness` | Yes | Full report |
| `tf_verify` | Yes + command checks | Full report with command results |
| `Stop` | Yes + command checks (if enabled) | Final report; gate decision (§8) |

---

## 6. MCP Tools & CLI Commands

### Changed Tools

#### `tf_start`
The agentic contract gains a `completeness` section (§7).

#### `tf_checkpoint`
The response embeds the existing `Checkpoint` (fields stay at the top level for backward compatibility) and adds `completeness`:

```json
{
  "id": "…", "summary": "…", "files": ["…"],
  "completeness": {
    "summary": { "open": 3, "satisfied": 2, "attested": 0, "waived": 0 },
    "newly_open": [
      { "obligation_id": "…", "describe": "Storybook stories for CheckoutPage",
        "expected": "src/features/payments/pages/CheckoutPage.stories.tsx",
        "severity": "required", "rule": "react-screen" }
    ],
    "newly_satisfied": ["Component test for CheckoutPage"],
    "still_open": ["E2E coverage for the CheckoutPage route"]
  }
}
```

The ClickUp progress comment gains one line: `**Completeness:** 2/5 · open: story, e2e, contract`.

#### `tf_stop`
`Service.Stop` changes from returning `error` to `(*StopResult, error)`, where `StopResult` carries the final completeness report and the gate outcome. Gate behavior is in §8.

### New Tools

| Tool | Arguments | Behavior |
|---|---|---|
| `tf_completeness` | `session_id` | Re-evaluate; return the full report including the rules that apply |
| `tf_attest` | `session_id`, `obligation_id`, `evidence` | Record the attestation (e.g. spec and test names); re-evaluate |
| `tf_waive` | `session_id`, `obligation_id`, `rationale` | Mark waived; calls `LogDecision(topic: "Completeness waiver: <describe>", status: rejected_alternative, files: [subject])` and links `waiver_decision_id` |
| `tf_plan` | `session_id`, `items: [{rule_id, vars}]` **or** `decline_reason` | Declare expected work before files exist (only for `plannable` rules); creates obligations, including one for the subject file itself, and syncs the ClickUp checklist immediately. With `decline_reason`, records that no plan applies (posted as a Decision) and stops the plan reminders |
| `tf_verify` | `session_id` | Run command checks now (requires `allow_commands`) |

### CLI

Each new tool gets a matching Cobra subcommand in `internal/cli/` (one file each, same as today), plus:

| Command | Purpose |
|---|---|
| `taskflow completeness <session_id>` | Print the report |
| `taskflow waive <session_id> <obligation_id> --rationale "…"` | Same as `tf_waive` |
| `taskflow attest <session_id> <obligation_id> --evidence "…"` | Same as `tf_attest` |
| `taskflow plan <session_id> --rule react-screen --var feature=payments --var name=CheckoutPage` | Same as `tf_plan` |
| `taskflow check [--session ID \| --project PATH] [--hook claude]` | Evaluate; exit status 2 if required obligations are open. `--project` finds the session via `.b1codes/session.lock`. `--hook` prints output in a specific agent tool's hook format (§8) |
| `taskflow stop … --force` | Human override for `block` mode; records an automatic Decision ("Completion forced by human with N open obligations") |

`--force` is **CLI-only**. The MCP `tf_stop` has no override, so an agent can get past the gate only by doing the work or waiving with a rationale.

---

## 7. Agentic Contract Additions

`AgenticContract` gains an optional `completeness` field (omitted when there is no manifest):

```json
"completeness": {
  "manifest": ".agents/completeness.yaml",
  "gate": "review",
  "stacks": [
    { "id": "web", "root": "frontend", "commands": { "test": "pnpm run test:ci", "storybook-test": "pnpm run test-storybook" } },
    { "id": "api", "root": "backend", "commands": { "test": "uv run pytest" } },
    { "id": "ios", "root": "ios", "commands": { "test": null }, "warning": "No test command: set stacks.ios.vars.scheme and destination in .agents/project/completeness.yaml" }
  ],
  "rules": [
    { "id": "react-screen", "describe": "New screen", "plannable": true,
      "requires": ["story (required)", "unit-test (required)", "e2e (required)",
                   "visual-regression (recommended)", "contract (required, if services changed)"] }
  ],
  "summary": { "open": 1, "satisfied": 4, "attested": 1, "waived": 1 },
  "open": [ { "obligation_id": "…", "describe": "…", "expected": "…", "severity": "required" } ],
  "inherited": [ { "obligation_id": "…", "describe": "…", "status": "waived", "from_session": "…" } ],
  "suggested_plan": {
    "reason": "Task description mentions \"screen\"",
    "rules": ["react-screen"],
    "example": { "rule_id": "react-screen", "vars": { "feature": "<feature>", "name": "<Name>Page" } }
  },
  "protocol": "Taskflow checks these at every tf_checkpoint and before tf_stop. If a requirement genuinely does not apply, call tf_waive with a rationale; it will be posted to the ClickUp task."
}
```

On a fresh start, `rules` puts the definition of done in front of the agent at the moment work begins. On resume, `open` restores exactly what was still owed, the same way `History.Snags` restores unresolved snags.

### Plan Suggestions

`suggested_plan` appears when a `plannable` rule's `plan_hints` match the ClickUp task name or description and the session's `plan_state` is `none`. Until the agent calls `tf_plan` (→ `planned`) or `tf_plan` with `decline_reason` (→ `declined`), every `tf_checkpoint` response repeats a one-line reminder. Nothing blocks on a missing plan: a wrong keyword match costs one decline call, not a stuck session.

---

## 8. Completion Gate

### Modes

Set by `[completeness] gate` in `config.toml`, which is a **floor**: the manifest's `gate:` can make it stricter (`report` < `review` < `block`) but never looser. The manifest lives in the repo the agent edits, so if it could lower the gate, an agent could get past `block` by editing (or breaking) the manifest. A project that wants `report` needs `report` in the user's config.

**When completeness can't be checked** (the manifest is invalid, the stored obligations can't be read, or the evaluation fails), the gate acts as if required work may be open. `block` refuses with `ErrCompletenessUnchecked` and changes nothing (a human can still use `--force`). `review` moves the task to review. `report` completes. An invalid manifest's own `gate:` can't be read, so the config gate applies.

| Mode | `Stop(completed)` with required obligations open |
|---|---|
| `report` | Completes normally; report included in the stop comment |
| `review` (default) | Completes locally (`COMPLETED`); ClickUp status → the `review` role (below) instead of `complete`; stop comment flags what's open |
| `block` | Returns `ErrIncompleteSession` listing open required obligations; no state change. The agent must satisfy, attest, or waive |

The gate applies only to the `completed` target. `paused` and `archived` (abandoning work) are never gated.

### Status Mapping by Lifecycle Role

**Existing bug this fixes.** ClickUp list statuses only have the types `open`, `custom`, `done`, and `closed`. Checked against the real "Development Task List" (list `901418247534`) on 2026-10-10:

| Status | Type | orderindex |
|---|---|---|
| to do | `open` | 0 |
| in progress | `custom` | 1 |
| in review | `custom` | 2 |
| testing/qa | `custom` | 3 |
| complete | `done` | 4 |
| cancelled | `closed` | 5 |

Today `Start` and paused `Stop` enqueue `status_type: "active"`, which never matches, so those updates fail until they hit the retry limit. `Stop(completed)` enqueues `"closed"`, which resolves to **cancelled**. The existing tests in `status_test.go` mock an `active` type, so they don't catch this.

**Fix: map events to lifecycle *roles* instead of types.** The `UPDATE_STATUS` payload changes from `{"status_type": …}` to `{"role": …}`, and `ResolveStatus` becomes `ResolveStatusForRole`:

| Role | Enqueued by | Default resolution (when not overridden) |
|---|---|---|
| `start` | `Start`; `Stop(paused)` | Lowest-`orderindex` `custom` status; else lowest `open` |
| `review` | `Stop(completed)` under the `review` gate with required work open | `custom` status whose name contains "review" (case-insensitive); else **no status change** |
| `complete` | `Stop(completed)` | First `done` status; else first `closed` |
| `archive` | `Stop(archived)` | First `closed` status |

Optional name overrides, matched against the list's cached statuses:

```toml
[clickup.status_map]
start    = "in progress"
review   = "in review"
complete = "complete"
archive  = "cancelled"
```

An override that doesn't exist on the task's list is logged and the default is used instead. The defaults resolve correctly for the list above with no config, so the clone → API key → `init` setup still works.

**Queue compatibility:** the worker treats any queued legacy payload `{"status_type": "active"}` as `start` and `"closed"` as `complete`, so ops already in the queue at upgrade time resolve correctly.

This fix doesn't depend on completeness and ships first (Phase 0, §16).

### Agent-Tool Hooks (Phase 4)

The MCP gate only fires if the agent calls `tf_stop`. An agent can also just stop working. Hooks close that gap:

- **Claude Code:** `b1 pair` installs a `Stop` hook that runs `taskflow check --project "$CLAUDE_PROJECT_DIR" --hook claude`. If required obligations are open, it prints Claude Code's block response with the open list as the reason, so the agent keeps working. When the hook input reports `stop_hook_active`, it allows the stop (exit 0), so a blocked agent can't get stuck in a loop.
- **Other agent tools:** a `--hook <name>` adapter per tool, where it supports one.
- **Fallback for any agent:** an optional `pre-push` git hook running `taskflow check --project .`.

Hooks run outside the model, so the agent can't skip them.

---

## 9. ClickUp Integration

### Checklist Sync

A new sync operation, `SYNC_CHECKLIST`, carries the **full desired state** rather than a single change:

```json
{ "checklist_name": "Taskflow · Definition of Done",
  "items": [ { "key": "react-screen/story/CheckoutPage", "name": "[story] Storybook stories for CheckoutPage", "resolved": false } ] }
```

The worker reconciles it:
1. `GET /task/{id}` → find the checklist by name. If it's missing, `POST /task/{id}/checklist`.
2. Match items by their stable `[requirement] describe` part (each payload item's `match`), not their full name: the name also carries a status suffix after ` — ` (verify levels and attestation, e.g. "[e2e] E2E coverage for CheckoutPage — attested, structural ✓"), which changes without the item becoming a different one. Exact names are matched first, and each ClickUp item is matched at most once, so obligations with the same text get one item each. Create any that are missing (`POST /checklist/{id}/checklist_item`) and update the name and `resolved` where they differ (`PUT /checklist/{id}/checklist_item/{item_id}`). An item is resolved when its obligation isn't open.
3. Don't delete items that are no longer present (`void` obligations); rename them to `[requirement] describe — void` instead (ClickUp item names are plain text, so no strikethrough), so history stays visible. A void obligation with no item yet gets none.

Since each op holds the full state, retries are safe, no ClickUp-returned IDs have to be stored, and the worker can skip a `SYNC_CHECKLIST` op when a newer one is pending for the same task.

It's enqueued when an evaluation changes any obligation's checklist item (its status, verify levels, or text), when `tf_waive`/`tf_attest` change one, and always on `tf_plan`. Evaluations that change nothing enqueue nothing.

New `clickup.Client` methods: `GetTaskChecklists`, `CreateChecklist`, `CreateChecklistItem`, `UpdateChecklistItem`.

### Comments

| Source | Comment |
|---|---|
| Checkpoint | Existing progress comment + one completeness line |
| `tf_waive` | Existing `POST_DECISION` format, via `LogDecision`; no new comment type |
| `tf_attest` | None. Attestations appear in the checklist item name and the stop report, which avoids comment noise |
| Stop | "Session Finished" comment gains a completeness table: each obligation, its status, its verify levels, and evidence or waiver rationale |

---

## 10. Data Model Changes

All changes are a new migration entry in `internal/store/migrations.go` (version 6; Phase 0 used versions 4 and 5 for the status-cache and task-cache cleanups). Existing entries are never edited.

### `sessions` (altered)

| Column | Type | Notes |
|---|---|---|
| `base_sha` | `TEXT` | Set on session creation; never updated on resume |
| `plan_state` | `TEXT NOT NULL DEFAULT 'none'` | `none`, `planned`, `declined` (§7) |

### `obligations` (new)

| Column | Type | Constraints | Notes |
|---|---|---|---|
| `id` | `TEXT` | `PRIMARY KEY` | UUID |
| `session_id` | `TEXT` | `NOT NULL REFERENCES sessions(id) ON DELETE CASCADE` | |
| `rule_id` | `TEXT` | `NOT NULL` | |
| `requirement_id` | `TEXT` | `NOT NULL` | |
| `subject` | `TEXT` | `NOT NULL` | Trigger file path |
| `source` | `TEXT` | | b1 module that contributed the requirement |
| `severity` | `TEXT` | `NOT NULL` | `required` / `recommended` |
| `describe` | `TEXT` | `NOT NULL` | Rendered text |
| `expected` | `TEXT` | | JSON: rendered paths/globs |
| `status` | `TEXT` | `NOT NULL` | `open`, `satisfied`, `attested`, `waived`, `void` |
| `verified_levels` | `TEXT` | | JSON array: `structural`, `command`, `attest` |
| `evidence` | `TEXT` | | Attestation text / truncated command output |
| `waiver_decision_id` | `TEXT` | `REFERENCES decisions(id)` | |
| `planned` | `INTEGER` | `NOT NULL DEFAULT 0` | Created by `tf_plan` |
| `inherited_from` | `TEXT` | `REFERENCES obligations(id)` | Source obligation when copied from a previous session (§5) |
| `reminded_count` | `INTEGER` | `NOT NULL DEFAULT 0` | Evaluations where it was returned as open |
| `first_seen_at` | `DATETIME` | `NOT NULL` | |
| `satisfied_at` | `DATETIME` | | |
| `updated_at` | `DATETIME` | `NOT NULL` | |

Indexes: `UNIQUE(session_id, rule_id, requirement_id, subject)`; `(rule_id, requirement_id)` for the drift report.

### Store Interface

`session.SessionStore` gains `SetSessionBaseSHA`, `SetSessionPlanState`, `UpsertObligation`, `ListObligations(sessionID)`, `UpdateObligationStatus`, `IncrementObligationReminders`, and `InheritObligations(fromSessionID, toSessionID)`. The drift-report aggregation query goes only on `store.Store`, since `session` doesn't need it.

---

## 11. Package Changes

Follows the existing inversion pattern: `session` defines the interfaces, and `adapters` implements them.

```
internal/
├── completeness/          # NEW — pure: manifest load/validate, pattern matching, check(), report types
├── session/
│   ├── service.go         # Start/Checkpoint/Stop call the evaluator; StopResult; gate
│   ├── completeness.go    # NEW — CompletenessEvaluator interface, Obligation type, merge logic, Waive/Attest/Plan
│   └── types.go           # Session.BaseSHA
├── adapters/
│   └── completeness.go    # NEW — implements session.CompletenessEvaluator via completeness + gitctx
├── gitctx/                # ChangedSince()
├── clickup/               # checklist endpoints; review-status resolution
├── sync/                  # SYNC_CHECKLIST op, coalescing
├── store/                 # migration v6, obligation CRUD, aggregation
├── server/                # tf_completeness, tf_attest, tf_waive, tf_plan, tf_verify; updated tf_checkpoint/tf_stop
└── cli/                   # completeness.go, waive.go, attest.go, plan.go, check.go; stop --force
```

```go
// internal/session/completeness.go
type CompletenessEvaluator interface {
    // Evaluate returns nil, nil when the project has no manifest.
    Evaluate(ctx context.Context, sess *Session, planned []*Obligation) (*Evaluation, error)
    RunCommands(ctx context.Context, sess *Session, obligations []*Obligation) ([]CommandResult, error)
}
```

`NewService` gains the evaluator as a nil-safe dependency, like `clickup`. A nil evaluator means completeness is off.

### Configuration

```toml
[completeness]
gate = "review"              # report | review | block
allow_commands = false
command_timeout_s = 300

[clickup.status_map]         # optional; all keys optional (§8)
# start = "in progress"
# review = "in review"
# complete = "complete"
# archive = "cancelled"
```

---

## 12. b1CodingTool Contract

These changes belong to the b1CodingTool repo and are tracked separately. Listed here because taskflow depends on what b1 produces.

1. **Modules may ship `completeness.yaml`** with rules in the §3 schema (no top-level `gate`). Rules split across modules by tool: `framework/react-web` owns screen detection, and new `testing/storybook`, `testing/playwright`, and `testing/contract` modules contribute requirements. A project only gets Storybook obligations if it uses the Storybook module.
   Each `framework/*` module also ships a `stack` block: its marker files, `vars`/`commands` detectors, and `env`, following the defaults table in §3. `testing/*` modules can add commands to a framework's stack (e.g. `storybook-test`).
   `b1 pair` emits one `regression-suite` rule per detected stack.
2. **Project overrides** in `.agents/project/completeness.yaml`: turn rules or requirements off by id, change severity, set `gate`, or add project-specific rules.
3. **`b1 pair` merges** module rules with the overrides into `.agents/completeness.yaml` (auto-generated header, `sources` filled in), and renders a **Definition of Done** section into `AGENTS.md` from the same rules:

   ```markdown
   ## Definition of Done (checked by taskflow)
   When you add a screen (`src/features/<feature>/pages/<Name>.tsx`):
   - **[required]** Storybook stories at `<Name>.stories.tsx`
   - **[required]** Component test at `<Name>.test.tsx`
   - **[required]** E2E coverage in `e2e/**/*.spec.ts`; report it with `tf_attest`
   - **[recommended]** Visual regression baseline
   - **[required, if services changed]** Contract tests in `tests/contract/`

   Taskflow checks these at every `tf_checkpoint` and before `tf_stop`. If one genuinely does not
   apply, call `tf_waive` with a rationale; it is posted to the ClickUp task as a decision.
   ```

4. **Phase 4:** `b1 pair` installs agent-tool hooks (§8) when the corresponding IDE/agent module is active.

Because both outputs come from one source, the agent's instructions and taskflow's checks can't disagree.

---

## 13. Drift Report: Feeding Gaps Back into the Agent Files

`taskflow completeness report [--project PATH] [--since 30d] [--format text|json|b1]` aggregates obligations per `(source, rule_id, requirement_id)`:

| Metric | What it says |
|---|---|
| Satisfied at first evaluation | `AGENTS.md` worked: the agent did it without a reminder |
| Satisfied after ≥1 reminder | Taskflow's reminders did the work, so the `AGENTS.md` guidance is weak |
| Waived (+ rationale clusters) | The rule may be too broad (e.g. "no screens behind feature flags need e2e") |
| Open at stop | The rule isn't being followed and needs stronger guidance, a stricter gate, or a hook |

`--format b1` prints markdown suggestions aimed at specific module context files ("`testing/playwright`: e2e satisfied only after reminders in 7 of 10 sessions — consider adding an example spec to `context/`"). Taskflow finds where the rules aren't being followed; b1 updates them. Nothing is changed automatically.

---

## 14. Error Handling

| Situation | Behavior |
|---|---|
| Manifest missing | Completeness off; no warnings |
| Manifest invalid (YAML or schema) | `Start` succeeds; contract carries `completeness.error`; checkpoint responses repeat it; the config gate applies, acting as if work may be open (§8): `block` refuses, `review` moves the task to review |
| Git unavailable / not a repo | Evaluate with structural `exists` checks only, against planned obligations; warning in report |
| `base_sha` unreachable | Merge-base fallback (§5); warning in report |
| Command check times out or fails | Obligation stays `open` with truncated output as evidence; never crashes `Stop` |
| No command resolves for a stack | Not a failure. The obligation falls back to attestation (`attest_when: no_command`); the report and contract carry a warning naming the override that fixes it |
| Stack `root` missing on disk | Stack skipped with a warning; its rules produce no obligations |
| ClickUp checklist sync fails | Normal retry semantics of the sync queue; local state is the source of truth |
| `tf_waive` / `tf_attest` on unknown or void obligation | Tool error naming valid open obligations |

As in the original spec, completeness never blocks local work because of ClickUp. The only thing that blocks is the `block` gate, and only for `Stop(completed)`.

---

## 15. Testing Strategy

- **`internal/completeness`:** table-driven tests over fixture manifests and synthetic changed sets: pattern captures, `exclude`, `only_if`, each `verify` primitive, `any_of`/`all_of`, template rendering, schema validation errors.
- **Command resolution:** fixture directories for every stack in the §3 defaults table: each lockfile → `{pm}`; `test:ci` beats `test`; uv/poetry/venv `{py}` prefixes; pytest config vs `manage.py test`; SwiftPM vs Xcode with and without vars. Also project-override pinning, an unresolved command → attestation fallback, and a monorepo with `frontend/` + `backend/` stacks where a backend-only diff creates only the `api` regression obligation.
- **`internal/gitctx`:** `ChangedSince` against temp repos (following the existing `gitctx_test.go` pattern): committed vs. uncommitted vs. untracked, renames, empty-tree base, unreachable base.
- **`internal/session`:** fake `CompletenessEvaluator`; merge semantics (sticky waivers, open ⇄ satisfied, void), `base_sha` kept on resume, each gate mode, `tf_waive` creating a Decision, reminder counting.
- **`internal/store`:** migration v6 on a fresh DB and on a v5 DB; obligation upsert uniqueness; aggregation query.
- **`internal/clickup` status roles:** fixtures that use the real ClickUp types (`open`/`custom`/`done`/`closed`, including the 6-status list in §8); each role's default, overrides, an override missing from the list, and the legacy `status_type` payloads. Replace the existing `"type": "active"` fixtures in `status_test.go`, since that type doesn't exist in ClickUp.
- **`internal/session` planning and inheritance:** `plan_hints` matching, `plan_state` transitions, reminders stopping after plan or decline; restart after `COMPLETED` copies open, attested, and waived obligations but not satisfied ones.
- **`internal/sync`:** `SYNC_CHECKLIST` reconcile against an `httptest` ClickUp (create, update, idempotent re-run, coalescing).
- **`internal/server`:** new tools' argument parsing and JSON shapes; `tf_checkpoint` response still exposes checkpoint fields at the top level.
- **End-to-end fixture:** a tiny React-shaped temp repo + manifest. Add a screen → open obligations → add files → satisfied → waive one → stop → assert the report and the queued sync ops.

---

## 16. Development Phases

| Phase | Scope | Deliverable |
|---|---|---|
| **Phase 0: Status fix** | Role-based `UPDATE_STATUS` payload, `ResolveStatusForRole`, `[clickup.status_map]`, legacy payload compatibility, corrected test fixtures | Completed sessions mark tasks *complete*, not *cancelled*; start/pause updates stop failing. No dependency on completeness; ship first |
| **Phase 1: Observe** | `internal/completeness` (manifest + structural checks + stack/command *resolution*, no execution), `gitctx.ChangedSince`, `base_sha` + `obligations` migration, evaluator adapter, completeness in `tf_checkpoint` responses + checkpoint comment line + stop report, `tf_completeness` / `taskflow completeness` | The agent gets timely reminders and ClickUp shows completeness. No blocking. A hand-written `.taskflow/completeness.yaml` works without b1 |
| **Phase 2: Enforce** | `tf_waive` (→ Decision), `tf_attest`, `tf_plan`, gate modes + review-status resolution, `StopResult`, `stop --force`, contract `completeness` section | Skipped requirements become ClickUp decisions; work outstanding at stop is visible in task status, or blocked |
| **Phase 3: Make it visible** | `SYNC_CHECKLIST` op, ClickUp checklist endpoints, coalescing | Live "Definition of Done" checklist on every task |
| **Phase 4: Harden** | Command *execution* (per-stack root and env) + `tf_verify`, `taskflow check --hook`, `deleted` triggers, drift report | Checks the agent can't skip, and a feedback loop into b1 modules |
| **b1 (parallel)** | Module `completeness.yaml`, project overrides, `b1 pair` manifest + Definition of Done rendering, then hook installation | One source of rules for agents and taskflow |

---

## 17. Resolved Decisions

Decided 2026-10-10. Each row lists the alternatives that were considered and rejected.

| # | Question | Decision | Rejected alternatives | Spec sections |
|---|---|---|---|---|
| 1 | How should up-front planning work, given diffs miss work that never happened? | **Suggest a plan, don't require one.** Obligations are inferred from the diff; `plan_hints` matches put a `suggested_plan` in the start contract, and checkpoints remind until the agent plans or declines | Diff-only (drop `tf_plan`); require a plan when hints match; always require a plan | §3 Rule Fields, §6 `tf_plan`, §7 Plan Suggestions, §10 `plan_state` |
| 2 | What does "regression" mean? | **The existing suite still passes, plus visual/interaction regression through stories.** A `regression-suite` rule per stack (command check; attested until commands are enabled) and a `visual-regression` requirement that the screen's stories have `play:`/`parameters:` (Storybook test-runner in Phase 4) | Playwright `toHaveScreenshot`; Chromatic; attest-only | §3 Schema, Rule Fields (`scope`), Requirement Fields (`attest_when`, `contains_in`) |
| 3 | Do a screen's child components each need stories? | **Yes, a story per new component.** The `react-feature-component` rule fires for every new file under `features/{feature}/components/`, excluding index, hook, story, and test files | Screen-level story only; required for core / recommended for feature-local; screen story must mention children | §3 Schema |
| 4 | How do lifecycle events map to ClickUp statuses? | **Roles with type-based defaults plus optional name overrides.** `start` → lowest `custom`; `review` → `custom` named "*review*" or no change; `complete` → `done` then `closed`; `archive` → `closed`. `[clickup.status_map]` overrides. Also fixes the existing `active`/`cancelled` bug | Type-based only, no review; explicit config required; per-list map built at `init` | §8 Status Mapping, §11 Configuration, §16 Phase 0 |
| 5 | What happens to obligations when a completed task is restarted? | **The new session inherits unresolved obligations and waivers.** `open`, `attested`, and `waived` rows are copied with `inherited_from`; satisfied rows aren't | Fresh start each session; obligations owned by the task instead of the session; inherit only open required obligations | §5 Inheritance on Restart, §7 `inherited`, §10 `inherited_from` |
| 6 | Should ClickUp tags inform plan suggestions? | **No.** `plan_hints` match only the task name and description. The current `phase:`/`platform:`/`tech:` tag scheme is being replaced with GitHub-style labels (`enhancement`, `bug`, …), so nothing depends on tags | Narrow suggestions by `platform:` tags | §3 Rule Fields |
| 7 | How is the test command chosen across frameworks? | **Resolved at runtime per stack.** b1 framework modules declare candidate commands with conditions (lockfiles, `package.json` scripts, config files); `b1 pair` records each stack's `root`; taskflow picks the first matching candidate on every evaluation. Project overrides can set vars or pin a command. Unresolved → attestation fallback with a warning | One fixed command per project; read `package.json` scripts only (doesn't cover Flutter, SwiftUI, or Python); resolve once at `b1 pair` time (goes stale when tooling changes) | §3 Stacks and Command Resolution, §4, §7 `stacks`, §12, §14 |

No open questions remain.
