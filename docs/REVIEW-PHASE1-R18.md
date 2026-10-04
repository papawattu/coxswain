# Phase 1 review, round 18: owner decisions

Decision round, 2026-10-02, since tag `review/phase1-r17`. No code findings.
This round records the owner's answers to the open questions in R16 and R17,
and to the samples-plan question raised on #44. The owner accepted the
reviewer's recommendation on each item (2026-10-02). Each item below gives the
decision and what it changes for the builder.

---

## Decisions

### D38 Q2 (R16): CNI-independent defence in depth

- [x] Decided (2026-10-01): accept the residual risk on verified CNIs with
  documented hardening now (#40), then the operator-side CNI self-test
  (#41 plan, #42 implementation, #43 follow-ups). All merged.

### D39 (R16): operator-injected hostnames resolve under the KubeArmor DNS allowlist

- [x] Confirmed as a house rule for every slice: full
  `<svc>.<ns>.svc.<clusterDomain>` names, `ndots:1` on the pod, the name on
  that pod's KubeArmor DNS allowlist, and a test pairing the injected name with
  the allowlist entry.

### D40 (R16): marking PRs ready

- [x] Decided: **(a) the builder marks its own PRs ready.** The owner grants
  the builder token `pull_requests:write`, fine-grained and scoped to this
  repository only.
- **Builder:** once the token works, run `gh pr ready <n>` yourself and keep
  the title free of "(DRAFT)". Until then, the reviewer keeps doing it and
  fixes the title before merging.
- **Acceptance:** a builder-run `gh pr ready` succeeds; AGENTS.md notes the
  flow.

### D44. CI: hosted runners for unit tests and lint

- [x] Implemented in 6ab1c62 (merge of #58)

**Decision:** move `make test` and `make lint` to **GitHub-hosted runners**
(cache the envtest binaries). The kind e2e runs (KubeArmor, Calico,
`make i42-e2e`, `make d38-cni-e2e`) stay local, with their evidence in the PR
body, as now. No self-hosted runner on the devbox.

**Acceptance:** every PR shows green `test` and `lint` checks from hosted
runners; a deliberately broken unit test turns the check red.

### D41 (R17): tool proxies

- [x] Decided:
  1. Tools are declared **on AgentPolicy** for now. A separate `ToolProxy`
     resource comes later, when tool credentials are managed centrally.
  2. **A proxy per tool per Loop**, matching today's isolation. Revisit with
     the D42 load-test numbers.
  3. Ship a **generic HTTP tool proxy** as the built-in default (D36), with
     typed extensions later.

### D42 (R17): fan-out

- [x] Decided:
  1. Gate batching: **one campaign-level plan approval** by default, plus a
     per-campaign flag to require approval per child.
  2. Proxy pooling: **load-test first**; if per-Loop cost is prohibitive,
     allow pooling only **within a tenant**, never across tenants.
  3. Job source: an **explicit list** first; a repo inventory later.

### D43 (R17): multi-tenancy

- [x] Decided:
  1. The first multi-tenant target is **Tier 1** (trusted internal teams).
     Keep the agent and proxy pod specs runtime-class and node-pool aware,
     so Tiers 2 and 3 are configuration.
  2. Ceilings: **strict intersection** now. Platform-approved exceptions,
     recorded in the ledger, come later.
  3. gVisor vs KubeArmor for Tier 2: **deferred** until there is a Tier 2 use
     case.

### Samples plan (#44): plan approval

- [x] Decided: **option B**. Plan approval stays automatic: no
  `AwaitingApproval` gate in the samples build. The bar becomes one task end to
  end on kind with the real vLLM: **Planning → Implementing → Verifying**, with
  evidence. S4 is the runner as phase driver plus the ADR-0004 reader; S4's PR
  updates `docs/SAMPLES-PLAN.md` to match.

### After the samples build

- [x] Decided: the next slice after S5 is **delivery**: push a branch and open
  a PR on the **in-cluster Gitea**, never GitHub.

---

## Owner actions (not builder work)

- Persist the inotify limits for kind (`fs.inotify.max_user_instances=512`,
  `fs.inotify.max_user_watches=524288`) in `/etc/sysctl.d/99-kind.conf`.
- D40: issue the builder token with `pull_requests:write` on this repository.
- Optional: a narrow permission rule in the builder's own settings allowing
  `rm -rf` only under `/tmp/tmp.*`, so its scratch checks stop waiting on
  prompts.
