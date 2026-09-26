# CLI + kubectl plugin plan (DRAFT)

The user wants **both** a standalone `cox` CLI **and** the `kubectl-cox` plugin.
They share one codebase so there's one source of truth for the commands and the
CRD mutations. The settled design constraint is preserved: **no operator HTTP
API** — every command talks directly to the Kubernetes API (via client-go) to
read/patch the Loop CRD and `kubectl exec`/`cp` into the sandbox. The operator
is never called over the network.

## Why two entry points

- **`cox`** (standalone): works from a dev box, in scripts/CI, and for the
  `memory export`/`import` flows that don't need kubectl. It reads kubeconfig
  itself.
- **`kubectl-cox`** (plugin): `kubectl cox <cmd>` — kubectl is already
  authenticated and the user is already in the kubectl UX. It's a thin shim over
  the same command set.

Both are one Go binary; the plugin is a symlink/alias so `kubectl cox …` and
`cox …` resolve to the same command tree.

## Shared command core

`cmd/cox/` — one `cobra` command tree (the user is comfortable with cobra, and
it's the standard for kubectl plugins). A package `internal/cli/` holds the
command definitions and the K8s client wiring; `cmd/cox/main.go` and
`cmd/kubectl-cox/main.go` are two thin `main`s that call the same tree.

**K8s client:** `client-go` via the in-cluster/kubeconfig context
(`k8s.io/client-go/tools/clientcmd`). No `sigs.k8s.io/controller-runtime`
client in the CLI (keep the CLI dep-light; it's a user-facing tool, not an
operator). The CLI builds a `rest.Config` from the ambient context and a
`dynamic`/`typed` client for the Loop CRD.

## Command set (one tree, both entry points)

Each command is a CRD mutation or a `kubectl exec`/`cp`. Same list as the
settled design, now with an owner per command:

| Command | Action | How it works |
|---|---|---|
| `cox apply -f loop.yaml` | create a Loop | `Create` the Loop object (kubectl-apply-equivalent) |
| `cox get [loops] [-n ns]` | list Loops | `List` Loops, print a table |
| `cox describe <loop>` | human-readable summary | `Get` Loop + print spec/status/history |
| `cox approve <loop> [--ns ns]` | set `PlanApproved` | `Update` a condition with PLAN.md sha256 |
| `cox reject <loop> --reason "..."` | set `PlanRejected` | `Update` condition + reason |
| `cox pause <loop>` / `cox resume <loop>` | set/clear `spec.suspend` | `Patch` the Loop spec |
| `cox logs <loop> [-f]` | stream sandbox logs | `kubectl exec` / pod logs of the sandbox Pod |
| `cox history <loop>` | print audit trail | `Get` Loop, render `status.history[]` |
| `cox diff <loop>` | workspace diff | `kubectl exec` `git diff` in the sandbox |
| `cox plan <loop>` | read the plan | `Get` Loop (`status.plan.summary`) + `kubectl cp` PLAN.md |
| `cox fork <loop> --from iter-N` | new Loop from checkpoint | `Get` checkpoint ref, `Create` a new Loop with `spec.restoreFrom` |
| `cox memory <repo-url> [list\|rm <id>]` | inspect/delete lessons | `kubectl cp` the memory dir out / patch the PVC-backed file (Phase 5) |
| `cox memory export <repo-url> -o file` | tarball the memory dir | `kubectl cp` + tar (Phase 5) |
| `cox memory import <repo-url> -f file` | restore a memory tarball | untar + `kubectl cp` (Phase 5) |
| `cox version` | print the CLI version | local |
| `cox completion [bash\|zsh\|fish]` | shell completions | cobra built-in |

**Phase split of the command set:**
- **Phase 1:** `apply`, `get`, `describe`, `logs`, `history`, `plan` (the read +
  create + exec subset that works once the CRD and a sandbox exist). This makes
  the CLI useful the moment Phase 1 lands, without waiting for Phase 4.
- **Phase 4:** `approve`, `reject`, `pause`, `resume` (the approval gate),
  `fork` (needs Phase 3 checkpoints), `diff`.
- **Phase 5:** `memory *` (shared memory).

## TDD seams for the CLI (Phase 1 subset)

The CLI's seams are the **K8s API** (the `rest.Config`/client) and the **kubeconfig
context**. Per the TDD rule, we mock system boundaries — but the K8s API is a
system boundary, so we mock *its client*, not the real cluster:

- **C1** — *`cox get` renders a table.* Given a fake client that returns a known
  Loop list, `cox get` prints the expected columns (NAME, PHASE, ITERATION, AGE).
  Seam: an injected `LoopLister` interface (mocked in the test); assert on the
  output text.
- **C2** — *`cox describe` renders spec+status+history.* Same seam; assert the
  output includes the goal, phase, and each history entry.
- **C3** — *`cox apply` creates the Loop.* Given a fake client that records
  `Create` calls, `cox apply -f loop.yaml` calls `Create` with a Loop whose
  spec matches the YAML. Seam: the fake client's recorded call; assert on the
  object.
- **C4** — *`cox pause` patches `spec.suspend`.* Fake client records a `Patch`;
  assert the patch sets `suspend: true` (and `resume` clears it).
- **C5** — *`cox approve` sets the PlanApproved condition with the PLAN.md
  sha256.* (Phase 4, but the seam is the same.) Fake client records an `Update`;
  assert the condition's `message` carries the hash.

The **client interface** (the seam) is a small set: `Get`, `List`, `Create`,
`Update`, `Patch` on Loops. The real impl wraps client-go; the test injects a
fake. The `kubectl exec`/`cp` commands (logs/diff/plan/plan.md) are harder to
unit-test (they shell out) — for those, the seam is "given this exec/cp runner,
assert the command line and the rendered output"; the actual exec is a thin
wrapper that the tests stub.

## What's NOT in this plan

- No operator HTTP API (settled). The CLI/plugin never talk to the operator
  over the network — only to the K8s API for the CRD and to the sandbox Pod via
  exec/cp.
- No authentication logic beyond kubeconfig — the user's existing kubeconfig /
  in-cluster config is the credential.
- No `cox` subcommand for running the model or the runner — that's the
  operator/runner's job.

## Settled design questions (2026-09-26, all confirmed with user)

1. **CLI framework = cobra.** Standard for kubectl plugins; completions +
   `kubectl cox` plugin support for free. One shared command tree in
   `internal/cli/`; two thin mains.
2. **Binary layout = two thin mains.** `cmd/cox/main.go` and
   `cmd/kubectl-cox/main.go` each call the same `internal/cli` tree. kubectl's
   plugin lookup just needs a binary named `kubectl-cox` on PATH.
3. **Phase 1 ships the read/create/exec subset:** `apply`, `get`, `describe`,
   `logs`, `history`, `plan`. The approval gate (`approve`/`reject`/`pause`/
   `resume`) is Phase 4; `fork` is Phase 3; `memory *` is Phase 5.

## CLI TDD seams (mocked, per the TDD rule)

The CLI's system boundary is the **K8s API client** (and kubeconfig). We mock
the client, not a real cluster:

- **C1** — `cox get` renders a table (fake client returns a known Loop list;
  assert columns NAME/PHASE/ITERATION/AGE).
- **C2** — `cox describe` renders spec+status+history (assert goal, phase, each
  history entry present).
- **C3** — `cox apply -f loop.yaml` calls `Create` with a Loop whose spec
  matches the YAML (fake client records the call; assert on the object).
- **C4** — `cox pause` patches `spec.suspend=true`; `resume` clears it (assert
  the patch).
- **C5** — (Phase 4) `cox approve` sets the PlanApproved condition with the
  PLAN.md sha256 (assert the condition's message carries the hash).

The **client interface** (the seam): `Get`/`List`/`Create`/`Update`/`Patch` on
Loops. Real impl wraps client-go; tests inject a fake. The `kubectl exec`/`cp`
commands (logs/diff/plan) are shelled out behind a thin runner interface that
tests stub (assert the command line + rendered output, not the real exec).

The **kubeconfig** boundary: the CLI builds a `rest.Config` from the ambient
context. In tests, the config is injected (a fake pointing at the fake client),
never the user's real kubeconfig.

