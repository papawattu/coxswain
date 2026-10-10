# Next phase: usable alpha

**Status:** accepted (PR #109). D52–D54 decided by the owner on 2026-10-10 (see the table at the end).

Status 2026-10-10, `main` at fa16581. PLAN.md Phases 0–2 are done, and
their Done-when conditions hold on kind. The core loop works end to end on
GitHub:
- clone into an isolated sandbox;
- plan → implement → verify, with protected-path tamper checks;
- stop on success, stall, budget or `maxIterations`;
- deliver a draft PR (I54, I72: papawattu/coxswain-sandbox#1, #2).

**What stops it being usable outside dev:**
- **Dev mode only:** it runs only with `--allow-unenforced`. Without real
  enforcement evidence (R22 I59), the fail-closed gate never opens.
- **No restarts:** what a Loop survives (operator restart, sandbox pod loss)
  has never been tested.
- **No handling tools:** there's no CLI to inspect or steer a Loop, and the
  delivered PR doesn't explain itself.
- **No install path:** there's no packaged install for anyone but us.

This phase closes exactly those gaps and nothing else (MVP alpha: feature-complete
before nice-to-have). It is inserted **between PLAN.md Phase 2 and Phase 3**;
the deferred items keep their PLAN.md homes.

**Done when (the alpha acceptance, A6):** on a fresh kind cluster, a user installs
coxswain from one released manifest **without `--allow-unenforced`**, applies a
Loop against a GitHub sandbox repo, and:
- the Loop reads `PolicyEnforced=True` on real evidence, and reaches Succeeded;
- it delivers a draft PR whose description explains the run;
- it survives a `kubectl delete pod` of the operator **and** of the sandbox
  mid-Implementing;
- the user can follow and steer it with `kubectl cox`.

## Slices (in order; one PR each, TDD per AGENTS.md test norms)

### A0. Finish the open R24 items
- **What:** I79 (evidence counts the metering proxy's usage lines, in
  progress) and I80 (D51: exclude an untracked root `PLAN.md` from the
  agent's commit).
- **Acceptance:** as written in R24 and #107.

### A1. Real enforcement evidence (R22 I59). Needs **D52**
- **Design first:** an ADR-0007 amendment choosing the evidence source.
  - **(a)** The KubeArmor relay alert stream (R9 I32's original plan). Strong
    evidence, but it adds a runtime dependency and a trust edge.
  - **(b)** A static check: the KubeArmor DaemonSet is Ready on the sandbox's
    node, the node reports BPF-LSM, and the Loop's KubeArmorPolicy exists and
    is accepted.
  - **Reviewer recommendation:** (b) for alpha. It's cheap, deterministic and
    fits the "operator reads facts" model. Then (a) as a later upgrade.
- **Either way:**
  - an outage or missing fact reads `Unknown`, never `True`;
  - the D30 gate opens only on `True`.
- **Acceptance:**
  - On kind-coxswain-dev, a Loop with its policy applied reads
    `PolicyEnforced=True` **without** `--allow-unenforced`.
  - Mutation: the check always fails → the Loop is held Suspended.
  - The per-input in-progress states (DaemonSet rolling, policy pending) hold
    the gate. This is the I49 norm.
- **Note:** the tool-proxy DNS fence stays a documented limitation (D50).

### A2. Restart durability baseline. Needs **D53**
- **Measure first:** a kind e2e that kills each component mid-run and records
  what happens:
  - the operator pod (in Planning, Implementing, Verifying, and with a deliver
    Job in flight);
  - the sandbox pod (mid-Implementing);
  - the verify Job pod.
- **Expected today:** the CR status and the workspace PVC survive, so many
  cases may already resume. The e2e turns "may" into evidence.
- **Then fix what it finds, including R24 I77:** a restart of a pod that still
  carries init-workspace must not wipe agent work.
- **D53 (scope):** for alpha, durability = **resume from the workspace PVC**.
  VolumeSnapshot checkpoints, retention, rollback and fork (PLAN.md Phase 3)
  move after alpha. The alternative is Phase 3 in full now, about 2 more weeks.
- **Acceptance:**
  - each kill case ends in the same terminal outcome as an uninterrupted run, or
    in a named, documented terminal reason, never a wedge;
  - every case is in the e2e, with kept logs.

### A3. `kubectl cox` minimal. Uses the draft `docs/CLI-PLAN.md`
- **The binary:** one cobra binary (`cox`, plus the `kubectl-cox` alias) that
  talks only to the Kubernetes API. There is no operator HTTP API (settled).
- **Alpha commands:**
  - `describe` (phase, iteration, conditions, budget, delivery);
  - `history` (`status.history[]`);
  - `logs` (agent, proxy, verify, deliver);
  - `plan` (`.coxswain/PLAN.md` from the workspace);
  - `pause` / `resume` (spec.suspend and the `coxswain.io/resume` annotation);
  - `redeliver` (the I52 annotation).
- **Deferred:** `approve`/`reject` (Phase 4), `fork` (Phase 3), `memory`
  (Phase 5), `diff`.
- **Acceptance:** a unit test per command against a fake clientset; one kind
  e2e that drives a Loop with `pause` → `resume` → `describe` → `redeliver`.

### A4. A self-explaining draft PR (PLAN.md Phase 6 remainder)
- **The PR description:** goal, iteration count, the verify results per
  iteration, the plan (from `.coxswain/PLAN.md`, so D51 still holds),
  budget used, and the Loop's namespace/name.
- **Generated operator-side** from status, so no runner claim is needed.
- **Acceptance:**
  - a live GitHub run shows the description;
  - a spec asserts the description is built from status fields only;
  - a mutation that sources it from `result.json` fails.

### A5. Packaging. Needs **D54**
- **The release:** a tagged `v0.1.0-alpha.1` with
  `dist/install.yaml` built by `make build-installer`. The operator, runner,
  proxy and egress-proxy images are pushed to a registry.
- **Docs:** a QUICKSTART covering prerequisites (agent-sandbox, KubeArmor with
  BPF-LSM, k8s version), a GitHub token Secret, a first Loop, and reading the
  result.
- **D54:**
  - which registry (ghcr.io/papawattu, or the homelab registry);
  - install.yaml only, or a Helm chart as well. Recommendation: install.yaml for
    alpha, Helm in Phase 7.
- **Acceptance:** A6 uses only the released artifacts.

### A6. Alpha acceptance (this phase's Done-when)
- **The script:** `make alpha-e2e` on a **fresh** kind cluster, following
  QUICKSTART literally.
- **Gate mutations:**
  - enforcement check failing → held;
  - sandbox kill → resumes;
  - delivery → one first-time PR.
- **Evidence:** kept, digests recorded (the AGENTS.md kind norms).

## Explicitly deferred (keep their PLAN.md homes)
- **Phase 3:** VolumeSnapshot checkpoints, rollback, fork (if D53 = PVC-resume).
- **Phase 4:** the Manual plan approval gate, reject/replan, `cox approve/reject`.
- **Phase 5:** shared memory and the Memory Curator.
- **Phase 6:** the Judge, `onVerifyFail: Plan`.
- **Phase 7:** metrics, Helm, demo video, CNCF name check.
- **Parked features:** the D45 observer, R17 D41–D43 (MVP-first).

## Owner decisions needed
| ID | Question | Recommendation |
|---|---|---|
| D52 | Enforcement evidence source (A1) | **Decided (owner, 2026-10-10): (b) the static check**; (a) relay later |
| D53 | Alpha durability scope (A2) | **Decided (owner, 2026-10-10): resume from the workspace PVC**; snapshots/rollback/fork after alpha (Phase 3) |
| D54 | Image registry; install.yaml vs Helm (A5) | **Decided (owner, 2026-10-10): ghcr.io + install.yaml**; Helm in Phase 7 |

## Estimate
A rough guess at the recent pace (pi building, reviewer gating every PR):
- A0: about 1 day;
- A1: 3–4 days including the design note;
- A2: 3–5 days, depending on what the kill e2e finds;
- A3: 2–3 days;
- A4: 1–2 days;
- A5: 1–2 days;
- A6: 1–2 days.

**About 2–3 weeks to the alpha tag.** Host memory limits kind runs to one at a
time, which is the main schedule risk.
