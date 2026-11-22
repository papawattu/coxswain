# Phase 9: Adoption (D130's second half)

> **Status:** PLANNED — not started. This is the "After Phase 8: adoption"
> track from `docs/PLAN-PHASE8.md:119-123`, written as its own plan per the
> plan's own note: *"This gets its own plan once v0.3.0 ships."*
>
> **Trigger:** v0.3.0/1/2 are shipped (tags `v0.3.0`, `v0.3.1`, `v0.3.2` on
> main; `docs/RELEASE-NOTES-v0.3.0.md` in tree). Adoption may begin.

---

## Goal

Move Coxswain from *proven on planted loops* to *used as the owner's day-to-day
agent driver on real repos*, and make the install path trustworthy for a
stranger. Three outcomes, from `PLAN-PHASE8.md` and `PLAN.md:107`:

1. **Dogfood** — Coxswain runs real goals on the owner's own repos day to day,
   not only planted ones. Each run's defects become slices.
2. **A QUICKSTART a stranger can follow** — proven by a fresh cluster set up
   from the document alone, every command pasted.
3. **The CNCF landscape** — an entry submitted by the owner (none exists, R56).

## Architecture / approach

Adoption is an *operational* phase, not a feature build. It reuses the
Phase-0–8 machinery (kind-ref, the canary note, the review loop, the planted
`Loop` samples in `.samples/dogfood*`) and adds: a standing dogfood harness on
owner repos, a defect→slice intake pipeline, a stranger-proof proof run, and a
landscape submission. No new controller code is expected from the plan itself;
slices created by defects land through the normal build→review→merge loop.

## Current context / assumptions

- **v0.3.0 is the adoption baseline.** The operator runs on kind-ref (kind on
  the devbox), images from ghcr. `arm64: built, not run` (D137) carries as the
  known caveat.
- **Planted loops are done.** `.samples/dogfood*` (11 dated runs, Oct 15–19)
  prove the harness end-to-end: Planning→Implementing→Verify, Judge advisory,
  scan, can-i, egress proxy, CNI fence. They are the *test* population, not the
  *adoption* population.
- **QUICKSTART is mostly there.** `docs/QUICKSTART.md` has 9 sections
  (Prerequisites, Install, GitHub Secret, First Loop, Reading results, Plan
  approval, Forking, Model tuning, Calico+KubeArmor VM). What is *not* yet
  proven is the **stranger** claim: a fresh cluster from the document alone,
  every command pasted, with a recorded transcript.
- **CNCF landscape:** R56 confirmed no existing "Coxswain" entry; the
  `coxswain.wattu.com` API group is under an owned domain. The entry itself has
  not been submitted.
- **Owner is the gate.** Per D139 and the merge gate, the owner tags, pushes
  the landscape entry, and approves real-repo runs. No auto-merge of adoption
  slices.

## Assumptions to confirm (open)

- **A1. Which owner repos?** The day-to-day dogfood target. **D140 = a new,
  simple web-based todo application** (created fresh for this phase). Rationale:
  a small, self-contained, well-understood app is the cleanest dogfood target —
  its defects are easy to root-cause (no existing complexity to confuse the
  signal), and the owner can verify the agent's PRs by eye against a working
  page. The repo starts empty; Coxswain's first real goal is to *build it* (a
  basic CRUD todo web app), and subsequent dogfood goals are incremental
  features/fixes on top (search, auth, persistence, etc.). This is a new repo,
  not an existing one — no risk of the agent's PRs colliding with real work.
- **A2. kind-ref vs. the live homelab cluster.** Per D142 the owner wants runs
  on the live homelab cluster, so dogfood targets **both**: kind-ref
  (isolated, fast, safe — the default for most goals) and the live homelab
  cluster (the real target the owner actually uses day-to-day). Live-cluster
  runs follow the guardrails in **A0.1** (dedicated namespace, resource caps,
  egress/secret scoping, owner gate per goal).
- **A3. Model.** The dogfood harness uses the configured model (qwen3.8-27b on
  the 5090 via vLLM) — the same as the build loop.
- **A4. Cadence.** One real-repo goal per day during the dogfood window, each
  producing a defect report. The owner sets the window (recommend 5 working
  days).

## Slices

### A0. Adoption harness on owner repos (dogfood scaffold)

**Objective:** A repeatable way to point Coxswain at a *real* owner-repo goal
(not a planted sample) and capture the full run as evidence.

**Files:**
- Create: `.samples/dogfood-real-<TS>/` (per run, mirroring the existing
  `.samples/dogfood*` layout: `loop.yaml`, `plan-attempt*.md`, `loop-final.yaml`,
  `events-final.txt`, `README.txt`).
- Create: `docs/DOGFOOD.md` — how to run a real-repo goal, what to capture,
  how to file a defect.

**Steps:**
1. Pick the first real goal (A1). Write a `Loop` CR whose `spec.goal` is a
   real task on the owner repo (not a planted verification).
2. Run it on kind-ref with the current tag (`v0.3.2` or latest).
3. Capture: `kubectl get loop` (with the Judge column), conditions, the plan
   attempts, the PR draft state, and the agent's run log.
4. Write `README.txt` in the run dir: goal, model, tag, outcome, defects found.
5. File defects as slices (see A1-defects).

**Verification:** A `Loop` with a real (non-planted) `spec.goal` runs
Planning→Implementing→Verify on kind-ref and produces a PR draft.

### A0.1. Live-homelab-cluster runs (D142)

**Objective:** Run real-repo dogfood goals on the live homelab cluster with
guardrails so a run cannot disturb owner workloads. The operator and the
dogfood Loop are already in-tree (the operator is installed on the cluster via
GitOps/Flux); this slice adds the *isolation* procedure, not new operator code.

**Guardrails (all required before any live-cluster run):**

1. **Dedicated namespace.** Each live-cluster dogfood run gets its own
   namespace (e.g. `dogfood-<goal>-<TS>`), not the owner's working namespaces.
   The Loop, its pods, its PVC, and its Secrets live there and nowhere else.
2. **Resource caps.** The Loop's agent/runner pods get explicit
   `resources.limits` (CPU + memory) and the namespace gets a `ResourceQuota`
   so a runaway run cannot starve owner workloads (vLLM on the 5090, ComfyUI,
   the operator itself). The quota is sized to the goal, not the cluster.
3. **Egress + secret scoping.** The run's egress proxy (J5b) and the GitHub
   token Secret are scoped to the run's namespace — the token's repo access is
   limited to the goal's repo, and egress is limited to the domains the goal
   needs (GitHub, the model endpoint). No run gets cluster-wide or
   cross-namespace access.
4. **NetworkPolicy / CNI fence.** The CNI fence (the probe's policy pass)
   enforces that the agent pod cannot reach owner workloads outside its
   namespace. This is the I265 rule, applied to the live cluster.
5. **Owner gate per goal.** The owner explicitly opts each live-cluster goal
   in (a `Loop` with a note "target: homelab"). No auto-targeting of the live
   cluster; kind-ref is the default, the live cluster is per-goal.
6. **Snapshot + rollback.** Before a run, note the namespace's current state
   (there should be none — it's fresh). After the run, the namespace is
   deleted (the Loop's finalizer cleans up its pods/PVC). A failed run that
   leaves orphaned resources is a defect (A1 intake).

**Files:**
- Modify: `docs/DOGFOOD.md` — the live-cluster procedure (steps 1–6 above).
- Create: `config/samples/dogfood-homelab.yaml` (or a per-run sample) — the
  Loop CR + ResourceQuota + NetworkPolicy for a live-cluster dogfood run,
  parameterised on the namespace.

**Steps:**
1. On the live cluster (via the homelab GitOps path — the owner's cluster is
   at `.21`, reached through the `papawattu/homelab` repo, not direct kubectl),
   create a fresh namespace for the run.
2. Apply the ResourceQuota + NetworkPolicy + egress-proxy + GitHub token
   Secret, scoped to that namespace.
3. Create the `Loop` with the real goal and `spec.goal` pointing at the
   owner repo.
4. Run it; capture the same evidence as A0 step 3.
5. On completion (or owner stop), delete the namespace and verify no orphaned
   resources remain.
6. File any defect (orphaned resource, quota not respected, egress leak) via
   A1 intake.

**Verification:** A real-repo goal runs on the live homelab cluster in a
dedicated namespace with a ResourceQuota + NetworkPolicy, completes (or is
stopped cleanly), and leaves no orphaned resources. The owner has opted the
goal in.

### A1. Defect→slice intake

**Objective:** Every dogfood run's defects become first-class slices.

**Files:**
- Modify: `docs/DOGFOOD.md` — the defect template.
- Create: `docs/ADOPTION-DEFECTS.md` — the running defect log (one section per
  run, each defect with: symptom, root cause, affected slice, proposed fix,
  status).

**Steps:**
1. For each defect: record the *symptom* (what broke), the *root cause* (which
   controller code path / spec / docs gap), and the *slice* (the file to
   change + the test that should fail).
2. Triage: a P1 defect (Loop stuck, wrong plan approved, security bypass)
   blocks further dogfood runs until a slice is built and merged.
3. Non-blocking defects (cosmetic, doc wording, UX) queue for the normal loop.

**Verification:** `docs/ADOPTION-DEFECTS.md` exists and references at least one
real defect with a concrete slice + test target.

### A1-defects. The defect slices

**Objective:** The actual fixes, each built→reviewed→merged through the normal
loop (owner is the merge gate).

**Files:** whatever `docs/ADOPTION-DEFECTS.md` names. No fixed file list — the
defects define it.

**Verification:** Each slice lands on main with its failing-test-then-passing
proof, and the referenced dogfood run is re-run to confirm the defect is gone.

### A2. Stranger-proof QUICKSTART

**Objective:** Prove `docs/QUICKSTART.md` is followable from nothing: a fresh
cluster, every command pasted verbatim, ending with a working Loop.

**Files:**
- Modify: `docs/QUICKSTART.md` — fix any command that does not paste-and-run
  (pinned versions, missing env, ordering gaps).
- Create: `docs/QUICKSTART-PROOF.md` — the recorded transcript: the exact
  commands pasted, their output (truncated), and the final `kubectl get loop`
  showing a completed Loop. The "Measured" evidence beside the canary note.

**Steps:**
1. Wipe kind-ref. Follow `docs/QUICKSTART.md` top to bottom, pasting each
   command exactly. Do not deviate; note every place a command failed or was
   ambiguous.
2. Fix the QUICKSTART for each failure (PR per fix, owner merge gate).
3. Re-run from the top until the whole document runs clean.
4. Record the transcript in `docs/QUICKSTART-PROOF.md`: every command + output
   (truncated), the tag used, the final `kubectl get loop` with the Judge
   column.
5. Add the proof's result to the canary note's "Measured" heading beside the
   E1 probe (`docs/CANARY-CHANNEL.md`).

**Verification:** A fresh kind cluster, built solely from `docs/QUICKSTART.md`
with zero improvisation, reaches a completed Loop. The transcript in
`docs/QUICKSTART-PROOF.md` matches the final QUICKSTART exactly.

### A3. CNCF landscape entry

**Objective:** Submit a "Coxswain" entry to the CNCF landscape.

**Files:**
- Create: the landscape repo's JSON/PR (upstream, not this repo).
- Modify: `docs/README.md` or `docs/OPERATOR.md` — a one-line link to the
  landscape entry once submitted.

**Steps:**
1. Re-confirm no existing "Coxswain" entry (R56 baseline; re-check at
   submission time).
2. Draft the entry: name (Coxswain), description (a Kubernetes operator that
   runs autonomous coding-agent loops on owner repos, with a plan-approval
   gate, an advisory Judge, a security scan, and network/LSM enforcement),
   category (Scheduling and Operations → or the closest fit), website
   (`https://github.com/papawattu/coxswain`), API group `coxswain.wattu.com`.
3. Owner submits the landscape PR (owner action per the plan).
4. On merge, record the entry URL in `docs/OPERATOR.md`.

**Verification:** The landscape PR is submitted by the owner; on merge the URL
is recorded in-tree.

## Parallel work / ordering

- **A0** runs first (it produces the defect population). **A1** (intake) is
  concurrent with A0's first run. **A1-defects** flow through the normal loop
  as they are filed — they are not gated on A2/A3.
- **A2** (stranger-proof QUICKSTART) is independent of A0–A1 and can run in
  parallel on a second kind-ref.
- **A3** (CNCF) is owner-gated and independent; it can be submitted at any
  point, but the landscape entry's description is more credible *after* A2's
  proof exists (it cites the proof).
- **Dogfood window (A4):** recommend 5 working days of A0 runs. The phase is
  "done" when A1-defects are drained (no open P1), A2's proof is green, and
  A3's entry is submitted.

## Done when

- [ ] A0: at least one real (non-planted) goal on the **new todo-app repo**
      runs Planning→Implementing→Verify and produces a PR draft (first on
      kind-ref; at least one on the live homelab cluster per A0.1, owner-opted);
      captured in `.samples/dogfood-real-<TS>/`.
- [ ] A1: `docs/ADOPTION-DEFECTS.md` exists; every P1 defect has a slice
      merged; the defect that spawned the slice is re-run and gone.
- [ ] A2: a fresh cluster built solely from `docs/QUICKSTART.md` reaches a
      completed Loop; transcript in `docs/QUICKSTART-PROOF.md` matches the doc;
      result added to the canary note.
- [ ] A3: CNCF landscape entry submitted by the owner; URL recorded in
      `docs/OPERATOR.md`.
- [ ] No open P1 defects; the dogfood window is closed.

## Explicitly deferred (keep their homes)

- The **oscillation detector** (D96), the **per-node CNI probe** (D80(b)), and
  **agent roles** (ADR-0010): unchanged — adoption does not build them.
- The **Judge as a gate** (J2): still advisory (D122). Dogfood runs keep the
  Judge advisory; no slice here promotes it to a gate.
- **arm64 hardware runs** (D137): still deferred. Dogfood runs on amd64
  (kind-ref on the devbox). The "built, not run" caveat carries.
- **Production homelab cluster use:** *not* deferred — per D142 the owner
  wants live-cluster runs. They follow the A0.1 guardrails (dedicated namespace,
  ResourceQuota, egress/secret scoping, CNI fence, owner gate per goal,
  snapshot+rollback). The default target is kind-ref; the live cluster is
  per-goal, owner-opted.

## Risks / tradeoffs

- **A real goal is open-ended.** A planted loop has a known answer; a real
  goal may take many iterations or produce a wrong-but-plausible plan.
  *Mitigation:* the plan-approval gate (QUICKSTART §6) means the owner reads
  the plan before Implementing advances — a wrong plan is caught there, not in
  the diff. Cap iterations per run (the Loop's existing iteration budget).
- **A P1 defect on a real repo.** If a defect blocks (e.g. a security
  bypass), the dogfood run stops and the slice is P1. *Mitigation:* kind-ref is
  isolated; the owner's repos are behind the GitHub token Secret with
  read/write scope limited to the run's namespace. **Live-cluster runs** (D142)
  add the A0.1 guardrails — dedicated namespace, ResourceQuota, egress/secret
  scoping, CNI fence — so a live run cannot starve owner workloads or reach
  cross-namespace resources. A live-run defect (orphaned resource, quota
  ignored, egress leak) is P1 and stops further live runs until fixed.
- **QUICKSTART drift.** The proof run (A2) is a snapshot; later merges can
  break a pasted command. *Mitigation:* the canary note (A2 step 5) is the
  standing check — a future run that diverges from the proof is flagged.
- **The landscape entry is owner-action.** The plan cannot submit it. *Note:*
  it is an owner push, like the tag (D139). The plan produces the draft and
  the exact submission steps; the owner fires it.

## Owner decisions

| ID | Question | Recommendation | Decision |
|---|---|---|---|
| D140 | Which owner repos for dogfood? | A new, simple web-based todo application (created fresh for this phase). | **As recommended — a new todo app** (owner, 2026-11-22). |
| D141 | Dogfood cadence / window? | 5 working days, one real goal/day, each → defect report. | (pending owner) |
| D142 | Does a real-repo dogfood run ever target the live homelab cluster? | Yes — the owner wants runs on the live homelab cluster. Live-cluster runs follow the A0.1 guardrails (dedicated namespace, ResourceQuota, egress/secret scoping, CNI fence, owner gate per goal, snapshot+rollback). | **Yes — live homelab cluster** (owner, 2026-11-22). See A0.1. |
| D143 | When to submit the CNCF entry? | After A2's proof is green (the entry cites it). Owner fires the PR. | (pending owner) |
