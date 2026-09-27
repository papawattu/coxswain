# Phase 1 review, round 6 — isolation is the product

Review of `cb7da6f` (2026-09-27), since tag `review/phase1-r5`, and a
correction to round 5's D26 after the owner's direction: **"the whole point
is the isolation."**

Each issue is self-contained so it can be picked up independently. Tick the
box and add the commit hash when done. Issue IDs continue project-wide.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `cb7da6f` | D25 `internal/tamper`, `:(glob)` pathspecs | OK | — |

D25 is closed properly: the tamper check is a real `git diff --name-only`
over a real temp repo, all globs carry `:(glob)`, user extras are wrapped,
and the eight fixtures (root and nested, add/edit/rename/delete, `go.mod`
`replace`, root `testdata/`, clean change) pass — with the plain-pathspec
failure on the three root cases demonstrated. Tick D25 in round 4.

Still open: **D27 (P1)** from round 5.

---

## P1 — Design: D26 revised

### D26 (revised). Coxswain isolates *any* agent; the agent holds no credentials

- [ ] Decided (ADR-0006) — **drafted** (0cafed9). ADR-0006 is committed against the revised D26 (agent-agnostic contract, zero credentials, proxy sidecar, egress deny-by-default, pod hardening, publish step, evil-agent e2e; isolation slices C1–C5 resequenced before B3). The **two open owner decisions** (first real agent to adapt; egress policy for dependency installs) are listed in ADR-0006 but NOT picked — the box stays unticked until the owner decides them.

**This supersedes round 5's D26 recommendation.** Round 5 framed the choice
as "which agent do we build" and recommended growing `runner/` first. The
owner's direction reframes it: Coxswain's value is not the agent — it's that
**any** coding agent (Claude Code, pi, Codex, OpenHands, our own `runner/`)
can be run **untrusted** inside a sandbox, and its output only counts if the
operator's own evidence says so. The agent is the payload; the isolation and
verification are the product.

What that means concretely — record in **ADR-0006** and plan it:

1. **Agent-agnostic contract.** A runner is *any image* that: reads its
   instructions (goal, iteration, feedback, `.coxswain/desired-phase`) from
   files in `/workspace/.coxswain/`, edits `/workspace`, makes git commits
   **locally**, and writes `.coxswain/result.json`. Nothing else. Adapters for
   existing agents are thin entrypoint scripts around their CLIs.
   `runner/` becomes the **reference/conformance** agent (and the one the
   tests drive with the fake model) — not the flagship.

2. **Zero credentials in the agent container.** Today's plan would mount the
   model key and a git push token into the sandbox. Both would then belong to
   whatever the model does with a shell. Instead:
   - **Model access via a sidecar proxy** in the sandbox pod: the agent talks
     to `http://localhost:<port>` (`COX_MODEL_BASE_URL`); the proxy holds the
     real key (mounted only into the proxy container), injects auth, forwards
     only to the configured endpoint, and **meters tokens** — this is Phase 2's
     metering sidecar, built now as the credential boundary.
   - **No push token in the agent.** The agent commits locally. A trusted
     component outside the agent's control (an operator-created "publish" Job,
     or a sidecar sharing only the workspace volume, read-only for the agent's
     `.git` refs it doesn't own) pushes the Loop branch after each iteration.
     That's also where D11's `verifiedCommit` gets pinned — the operator
     publishes and pins the same SHA, so the agent can't force-push after
     verify.
   - `automountServiceAccountToken: false` on the sandbox pod; no Kubernetes
     credentials at all.

3. **Egress deny-by-default.** A NetworkPolicy on the sandbox allowing
   egress **only** from the proxy container to the model endpoint (plus DNS).
   The agent container reaches nothing but `localhost`. Package installs
   (`go mod download`, `npm install`) are the hard case — decide between a
   pre-warmed module cache in the image, a caching proxy the policy allows, or
   an explicit `spec.agent.egressAllow[]`. Record the choice; don't open
   general internet egress by default.

4. **Pod hardening, on by default.** `runAsNonRoot`, drop all capabilities,
   `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, read-only
   root filesystem with writable `/workspace` + scratch, CPU/memory limits,
   and a `runtimeClassName` (gVisor/kata) when the cluster offers one —
   agent-sandbox's `SandboxTemplate` is the natural place to carry this.

5. **Loop API.** `spec.agent: { image, model, endpointSecretRef, env?,
   egressAllow? }` — the secret ref is mounted into the **proxy**, never the
   agent. Cluster defaults via a ConfigMap so the README sample stays short.

6. **Tests that prove isolation**, not just behavior: envtest asserts the
   built Sandbox pod spec has no token automount, no secret volume in the agent
   container, the hardening fields set, and the NetworkPolicy present; the e2e
   runs an "evil agent" image that tries to read a secret, reach the API
   server, curl the internet, and push to the remote — each must fail, and
   the attempts must not affect the Loop's evidence.

**Resequence:** after D27, the next slices are **sandbox hardening + proxy
sidecar + NetworkPolicy + publish step + evil-agent e2e**, then A1–A4 on the
reference runner, then B3. The isolation guarantees are what make B2/B3's
evidence meaningful; building verify before isolation verifies nothing.

**Owner decisions needed (don't pick silently):**
- The first real agent to adapt (Claude Code, pi, Codex, OpenHands, …).
- The egress policy for dependency installs (item 3).

**Acceptance:** ADR-0006 committed; `TDD-PLAN-PHASE1.md` gains the isolation
slices (each with a seam) ahead of B3; round 5's D26 box marked "superseded by
R6 D26".

---

## Next round will check

- D27 (P1).
- ADR-0006 draft and the resequenced plan.
