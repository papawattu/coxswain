# Glossary

Plain-language definitions of the terms an operator meets in `docs/`.
Alphabetical; each entry links to where the docs define the term.

- **Acceptance checks** — The user-authored shell commands
  (`spec.verify.acceptanceChecks`) the verify Job runs to decide the Loop:
  exit 0 = pass. They are the only *automated* gate — a human reviewing the
  draft PR is separate.
  See: [ADR-0005, Decision](adr/0005-operator-verifies-via-isolated-job.md#decision),
  [QUICKSTART §4 (What you will see, in order)](QUICKSTART.md#4-a-first-loop).

- **Agent** — The AI process that does the work inside the sandbox: it clones
  the workspace, writes the plan, implements the goal, and reports its phase
  claim. Its log shows planning, implementing, and the model calls.
  See: [OPERATOR.md §Debugging a stuck Loop (3. Read the agent
  log)](OPERATOR.md#debugging-a-stuck-loop).

- **AgentPolicy** — A separate CRD
  (`agentpolicies.coxswain.wattu.com`) declaring, per policy, what an agent
  may do: additive allows over default-deny, including network allows
  (host:port) and `tools[]` that spawn tool proxies. The Loop references it;
  the effective policy is what the operator enforces.
  See: [OPERATOR.md §Verify the
  install](OPERATOR.md#verify-the-install),
  [ADR-0007, Q2](adr/0007-agentpolicy-default-deny-ebpf-audit.md#q2--agentpolicy-its-own-crd-default-deny-additive-allows).

- **Approval gate** — The optional manual gate (`spec.approval.mode: Manual`):
  the Loop pauses at `AwaitingApproval` after Planning, the user reads the
  plan and runs `cox approve` (PlanApproved, hash-locked) or `cox reject`
  (PlanRejected; the `onReject` action is Replan or Fail). Off by default.
  See: [QUICKSTART §6 (Plan
  approval)](QUICKSTART.md#6-plan-approval-manual-approval-gate).

- **Base commit** — The commit the operator resolves from
  `spec.workspace.ref` at Loop start and pins in `status.baseCommit`;
  immutable for the Loop's life, and one endpoint of the tamper diff.
  See: [ADR-0005, Decision](adr/0005-operator-verifies-via-isolated-job.md#decision).

- **Budget** — The caps on a Loop (`spec.budget`: tokens, wall-clock, cost)
  and the accumulated usage folded into `status.budget` from the model
  proxy's metered reading; hitting a cap gives `BudgetExceeded` (or
  `Paused=True reason=Budget` with `onExceeded=Pause`).
  See: [OPERATOR.md §Key conditions to
  watch](OPERATOR.md#key-conditions-to-watch),
  [OPERATOR.md §Debugging a stuck Loop
  (1)](OPERATOR.md#debugging-a-stuck-loop).

- **Checkpoint** — A VolumeSnapshot of the workspace taken per iteration
  (best-effort by default; `spec.checkpoints: Required` fails the Loop at
  start if snapshots are impossible). Checkpoints are what forks and
  rollbacks restore from, and what the Loop's finalizer protects.
  See: [QUICKSTART §7 (Forking a
  Loop)](QUICKSTART.md#7-forking-a-loop),
  [OPERATOR.md §Uninstall](OPERATOR.md#uninstall).

- **Curator** — The Memory Curator LLM invoked by the operator only after all
  static rules pass, to make the single judgment call "is this lesson worth
  keeping?"; its verdict (boolean + one-line reason) is written to the audit
  trail and it never rewrites the lesson text.
  See: [ADR-0002,
  Context](adr/0002-llm-curation-rule-gated.md).

- **Deliver Job** — The operator-owned Job that opens the Loop's PR through
  the Gitea/GitHub API using the `git-cred` basic-auth Secret in the Loop's
  namespace; a failed delivery is recorded as `Delivered=False
  reason=DeliveryFailed` and retried via redeliver.
  See: [QUICKSTART §3 (The GitHub token
  Secret)](QUICKSTART.md#3-the-github-token-secret),
  [OPERATOR.md §Debugging a stuck Loop (7.
  Redeliver)](OPERATOR.md#debugging-a-stuck-loop).

- **Delivered condition** — The condition that tracks delivery: True when
  the PR is open, False with reason `DeliveryFailed` when the deliver Job
  failed; the Loop is held at `Verifying` until it is terminal.
  See: [OPERATOR.md §What `Succeeded`
  means](OPERATOR.md#what-succeeded-means-p5f-i98).

- **Egress proxy** — A per-Loop, operator-owned proxy that carries the
  sandbox's egress to the outside world and enforces the `host:port`
  allowlist at the HTTP CONNECT / SNI / Host layer; the sandbox's egress
  NetworkPolicy points only at it (plus DNS, the model proxy, and tool
  proxies).
  See: [OPERATOR.md §What the sandbox can
  reach](OPERATOR.md#what-the-sandbox-can-reach),
  [ADR-0007, I42 resolution](adr/0007-agentpolicy-default-deny-ebpf-audit.md#i42-resolution-the-egress-proxy-owner-decision-2026-09-28).

- **Escape hatches** — The two dev-only manager flags, independent of each
  other: `--allow-unenforced` (disables the KubeArmor policy gate) and
  `--allow-unenforced-network` (lets Loops run on a non-enforcing CNI,
  `NetworkEnforced=False reason=EnforcementDisabled`). Never for production.
  See: [QUICKSTART §1 (Network gate (D38))](QUICKSTART.md#network-gate-d38).

- **Fork** — A new one-shot Loop seeded from another Loop's checkpoint: it
  inherits the source's spec (optionally with `--check` / `--goal`
  overrides) and the operator restores the fork's own workspace PVC from the
  source's snapshot. A fork runs a normal one-shot Loop; it does not re-task
  the source.
  See: [QUICKSTART §7 (Forking a
  Loop)](QUICKSTART.md#7-forking-a-loop),
  [ADR-0003, Context](adr/0003-one-shot-loop-fork-not-retask.md).

- **Gate** — A fail-closed precondition the operator checks before letting a
  Loop run: the network gate (the CNI must enforce NetworkPolicy, probed by
  a self-test pod; unsatisfied ⇒ Loop held Suspended), the KubeArmor policy
  gate (KubeArmor + BPF-LSM must be enforcing; unsatisfied ⇒
  `PolicyEnforced=False`), and the stall gate (k consecutive identical verify
  failures ⇒ Stalled).
  See: [QUICKSTART §1 (Network gate
  (D38))](QUICKSTART.md#network-gate-d38),
  [QUICKSTART §1 (KubeArmor)](QUICKSTART.md#kubearmor-bpf-lsm-for-the-policy-gate--required),
  [OPERATOR.md §Key conditions to watch](OPERATOR.md#key-conditions-to-watch).

- **`git-cred` Secret** — The `kubernetes.io/basic-auth` Secret (username
  `x-access-token`) in the Loop's namespace that the deliver Job uses for
  the git push and the PR-create API call; if it is missing the deliver Job
  fails and the supported way back is redeliver.
  See: [QUICKSTART §3 (The GitHub token
  Secret)](QUICKSTART.md#3-the-github-token-secret).

- **History** — The Loop's audit trail (`status.history[]`, read with
  `cox history`): the phase + verify-failure history ring, including
  per-iteration `verifiedCommit`.
  See: [QUICKSTART §5 (Reading results with
  `kubectl cox`)](QUICKSTART.md#5-reading-results-with-kubectl-cox),
  [OPERATOR.md §Reading Loop status](OPERATOR.md#reading-loop-status).

- **Iteration** — One pass of the implement → verify cycle; each iteration
  is recorded in the `status.iterations` ring, and the Loop stops at its
  `maxIterations` budget if verify keeps failing.
  See: [QUICKSTART §4 (What you will see,
  in order)](QUICKSTART.md#4-a-first-loop),
  [OPERATOR.md §Reading Loop status](OPERATOR.md#reading-loop-status).

- **KubeArmorPolicy** — The KubeArmor object (the A1 policy gate, ADR-0007)
  the operator emits selecting a Loop's sandbox pod, owned by the Loop, that
  enforces the agent policy (exec/process allow and deny, egress); without
  KubeArmor the policy gate cannot be enforced and the alpha is not running
  as designed.
  See: [QUICKSTART §1 (KubeArmor (BPF-LSM for the policy
  gate))](QUICKSTART.md#kubearmor-bpf-lsm-for-the-policy-gate--required),
  [ADR-0007, Q3](adr/0007-agentpolicy-default-deny-ebpf-audit.md#q3--ebpf-enforcement-kubearmor-in-bpf-lsm-mode).

- **Lesson** — A short note the agent proposes (in `result.json`'s
  `lessons[]`) that survives static rules (≤500 chars, references a file
  path or error signature, no secrets, dedup) and the curator's judgment,
  then gets written to the memory PVC. Dropped lessons show up as
  `curator-drop` or `static-drop` in `coxswain_memory_lessons_total`.
  See: [ADR-0002,
  Context](adr/0002-llm-curation-rule-gated.md),
  [OPERATOR.md §The B5a metrics](OPERATOR.md#the-b5a-metrics).

- **Loop** — A one-shot, single-purpose unit of work: a `Loop` CR whose phase
  machine drives an agent through planning, implementation, and verification
  until it passes its acceptance checks or hits a limit. Loops are never
  re-tasked mid-flight; a changed goal is a new Loop or a fork.
  See: [QUICKSTART §4 (A first
  Loop)](QUICKSTART.md#4-a-first-loop),
  [ADR-0003, Context](adr/0003-one-shot-loop-fork-not-retask.md).

- **Memory** — The shared, per-(namespace, repo URL) ReadWriteMany PVC
  holding at most 100 lessons per PVC (a memory-writer Job writes back when
  a Loop ends `Succeeded` or `Failed:Stalled`, evicting the oldest by mtime
  when the cap is exceeded); a memory reader seeds new Loops from it, reading
  each lesson file at most 2000 bytes and the whole "SHARED MEMORY" prompt
  section at most 16 KiB.
  Without it a Loop simply runs without seeded lessons.
  See: [OPERATOR.md §Shared memory storage
  (Phase 5)](OPERATOR.md#shared-memory-storage-phase-5),
  [OPERATOR.md §Memory PVC lifecycle and
  removal](OPERATOR.md#memory-pvc-lifecycle-and-removal-i101).

- **Metering** — The model proxy's report-only usage accounting (tokens
  prompt/completion), which the operator folds into `status.budget` and
  exposes as `coxswain_loop_tokens_total`; it is the only place model token
  consumption is measured.
  See: [ADR-0009,
  Context](adr/0009-metering-in-model-proxy.md#context),
  [OPERATOR.md §The B5a metrics](OPERATOR.md#the-b5a-metrics).

- **Model proxy** — A separate operator-owned pod (`<loop>-proxy`) outside
  the agent's control that is the agent's only route to the model endpoint;
  it holds the model key (the Secret is mounted into the proxy pod, never
  the agent) and reports metered usage back to the operator.
  See: [OPERATOR.md §What the sandbox can
  reach](OPERATOR.md#what-the-sandbox-can-reach),
  [ADR-0009, Context](adr/0009-metering-in-model-proxy.md#context).

- **Network gate (D38)** — The network-layer gate requiring a CNI that
  enforces NetworkPolicy (Calico is the tested profile): the operator probes
  it with a self-test pod in `coxswain-cni-probe`, and a non-enforcing CNI
  holds all Loops Suspended, fail-closed, unless the dev escape hatch
  `--allow-unenforced-network` is set.
  See: [QUICKSTART §1 (Network gate (D38))](QUICKSTART.md#network-gate-d38).

- **Paused** — The Loop phase held when a stall or budget event has
  `stallAction`/`onExceeded` = Pause: the Loop stays Paused until
  `cox resume` (which for a Stall/Budget pause also sets the
  `coxswain.io/resume` annotation to clear the pause condition).
  See: [OPERATOR.md §Debugging a stuck Loop
  (6)](OPERATOR.md#debugging-a-stuck-loop),
  [QUICKSTART §5 (Reading results with
  `kubectl cox`)](QUICKSTART.md#5-reading-results-with-kubectl-cox).

- **Phase claim** — The runner's self-report of which phase it claims,
  delivered through the agent container's termination message, which the
  operator reads with a plain pod Get — there is no `pods/exec` grant and the
  operator never execs into the sandbox.
  See: [OPERATOR.md §How the operator reads the
  sandbox's self-report](OPERATOR.md#how-the-operator-reads-the-sandboxs-self-report).

- **Phases** — The Loop's state machine: Pending / Planning / Implementing /
  Verifying / Succeeded / Failed / Paused (plus AwaitingApproval when the
  manual approval gate is on). `Succeeded` means verified AND delivery is
  terminal (or delivery is off).
  See: [OPERATOR.md §Reading Loop
  status](OPERATOR.md#reading-loop-status),
  [OPERATOR.md §What `Succeeded`
  means](OPERATOR.md#what-succeeded-means-p5f-i98).

- **Plan** — The plan the agent writes at `.coxswain/PLAN.md` in the
  workspace, read with `cox plan`; with the approval gate on, the plan's
  SHA-256 is what the approval pins, and a plan change after approval
  invalidates the approval.
  See: [QUICKSTART §6 (Plan
  approval)](QUICKSTART.md#6-plan-approval-manual-approval-gate).

- **Policy gate (A1)** — The KubeArmor enforcement gate: the Loop's
  KubeArmorPolicy must exist and match the sandbox pod and BPF-LSM must be
  enforcing; without it the alpha is not running as designed.
  See: [QUICKSTART §1 (KubeArmor (BPF-LSM for the policy
  gate))](QUICKSTART.md#kubearmor-bpf-lsm-for-the-policy-gate--required),
  [ADR-0007 Amendment A1,
  Decision](adr/0007-amendment-a1-static-enforcement.md#decision).

- **Preset** — The explicit `spec.verify.preset` enum (default `go` in
  Phase 1) that expands to the language's default protected-glob set,
  because the content-free operator cannot detect the repo's language.
  See: [ADR-0005, Protected-path
  preset](adr/0005-operator-verifies-via-isolated-job.md#protected-path-preset-d16),
  [QUICKSTART §7 (Forking a Loop)](QUICKSTART.md#7-forking-a-loop).

- **Protected globs** — The path patterns the tamper check diffs against
  (base commit → verified commit): the language preset's glob set plus
  `spec.verify.protectedPaths[]` (or `protectedPathsOverride` /
  `preset: none` to replace it). Any change under a protected path is
  `TamperedVerify`, terminal.
  See: [ADR-0005, Protected-path
  preset](adr/0005-operator-verifies-via-isolated-job.md#protected-path-preset-d16).

- **Redeliver** — The `coxswain.io/redeliver` annotation (set by
  `kubectl cox redeliver`) that makes the operator re-run the deliver Job for
  a Loop that verified but failed to open its PR.
  See: [OPERATOR.md §Debugging a stuck Loop (7.
  Redeliver)](OPERATOR.md#debugging-a-stuck-loop).

- **Rollback** — Restoring a Loop's workspace PVC from an earlier checkpoint
  via `spec.restoreFrom`, which names the iteration to restore and is
  immutable (a different rollback target is a fork).
  See: [TROUBLESHOOTING.md §Editing `spec.restoreFrom` is
  rejected](TROUBLESHOOTING.md#editing-specrestorefrom-is-rejected-by-admission-f35).

- **Runner** — The agent's program image, as a distinct component name: the
  runner plans, implements, drives the phases, and reports its phase claim
  through the agent container's termination message. "Agent" and "runner"
  are the same thing in the docs' usage (e.g. "the agent (runner) log").
  See: [OPERATOR.md §Component
  logs](OPERATOR.md#component-logs),
  [OPERATOR.md §How the operator reads the
  sandbox's self-report](OPERATOR.md#how-the-operator-reads-the-sandboxs-self-report).

- **Sandbox** — The isolated agent-sandbox workspace pod for each Loop: the
  operator creates the Sandbox and the agent-sandbox controller starts the pod.
  It runs the agent container only; the model, egress and tool proxies are
  separate pods. If the pod is not Running, the Loop is stuck at the sandbox
  level.
  See: [QUICKSTART §1 (agent-sandbox)](QUICKSTART.md#agent-sandbox),
  [OPERATOR.md §What the sandbox can and cannot
  reach](OPERATOR.md#what-the-sandbox-can-and-cannot-reach).

- **Stall gate** — The gate that fires when the verify Job reaches k
  consecutive identical check outputs: `Failed=True reason=Stalled`, or
  `Paused=True reason=Stall` with `stallAction=Pause`. The stall history is
  in `status.stallHistory`. Verify *infrastructure* failures (a trusted init
  container, or a Failed Job with no check run) are a different verdict: they
  are retried twice (three total runs) and then fail the Loop with the
  distinct reason `VerifyInfraFailed`; they never feed the stall gate (only
  the failing check's output does).
  See: [OPERATOR.md §Key conditions to
  watch](OPERATOR.md#key-conditions-to-watch),
  [OPERATOR.md §The B5a metrics](OPERATOR.md#the-b5a-metrics).

- **Suspended** — The sandbox state while a fail-closed gate is not
  satisfied or the Loop is paused: on a non-enforcing CNI the sandbox is
  held Suspended with `NetworkEnforced=False reason=CNIUnenforced`; while
  delivery runs after a passing verify the sandbox is also held Suspended.
  See: [QUICKSTART §1 (Network gate
  (D38))](QUICKSTART.md#network-gate-d38),
  [OPERATOR.md §What `Succeeded`
  means](OPERATOR.md#what-succeeded-means-p5f-i98).

- **Tamper check** — The verify Job's init-container check
  (`git diff --name-only <baseCommit> <verifiedCommit> -- <protected globs>`
  in a trusted image, no agent code runs): non-empty ⇒ `TamperedVerify`. It
  catches added, modified, deleted, and renamed protected files.
  See: [ADR-0005, Decision](adr/0005-operator-verifies-via-isolated-job.md#decision).

- **Tool proxy** — A credentialed per-tool, per-Loop proxy (a generic HTTP
  tool proxy is the built-in default) that lets the agent use a declared tool
  without the agent holding that tool's credential; each gets its own
  two-layer allowlisted egress and its own egress peer in the agent's
  NetworkPolicy.
  See: [ADR-0008,
  Decision](adr/0008-tool-proxies.md#decision),
  [OPERATOR.md §What the sandbox can
  reach](OPERATOR.md#what-the-sandbox-can-reach).

- **Verify Job** — The operator-owned Kubernetes Job that runs the acceptance
  checks in isolation, with the tamper check as its own init container; the
  Loop is undecided while the Job is still Running, and the Job's log is the
  evidence for why checks failed.
  See: [OPERATOR.md §Debugging a stuck Loop
  (2)](OPERATOR.md#debugging-a-stuck-loop),
  [ADR-0005, Decision](adr/0005-operator-verifies-via-isolated-job.md#decision).

- **Verified commit** — The commit the operator resolves from the Loop branch
  at `Verifying` start and pins in `status.history[n].verifiedCommit` (D11);
  the other endpoint of the tamper diff, which the Job checks out before
  running checks.
  See: [ADR-0005, Decision](adr/0005-operator-verifies-via-isolated-job.md#decision).

- **`kubectl cox`** — The operator-facing CLI for reading and steering Loops:
  `describe`, `history`, `plan`, `diff`, `approve`/`reject`, `pause`/`resume`,
  `redeliver`, `fork`, `logs` (agent, proxy, verify, deliver), and `memory`
  (`import`, `export`, `delete`). It talks to the API server with the Loop
  CRD and uses `kubectl exec`/`cp` for workspace reads.
  See: [CLI-PLAN.md, Command set
  (one tree, both entry points)](CLI-PLAN.md#command-set-one-tree-both-entry-points),
  [OPERATOR.md §Component logs](OPERATOR.md#component-logs).
