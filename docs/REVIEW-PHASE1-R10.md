# Phase 1 review, round 10 — open issues

Review of `ac075c1`..`4a621d8` (2026-09-27), since tag `review/phase1-r9`:
slice **C1** (sandbox pod hardening + `spec.agent`) and the ADR-0007
amendments for round 9.

Each issue is self-contained so it can be picked up independently. Work
test-first. Tick the box and add the commit hash when done. Issue IDs
continue project-wide.

Priority: **P1** = fix before the next slice builds on it. **P2** = decide
during Phase 1. **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `ac075c1` | C1 pod hardening + `spec.agent` | **CHANGES** | **I34, I35 (P1)**, I36, I37, I38 |
| `19ef081` | ADR-0007 amendments (D29–D32, I32, I33) | OK | — |
| `af516a0` | bookkeeping (R9) | OK | — |
| `4a621d8` | C1 generated deepcopy | OK + note | I38 |

The ADR-0007 amendments close round 9 as asked: per-container egress via
eBPF with the NetworkPolicy as the outer fence, fail-closed
`PolicyEnforced=False` (with the terminal-vs-wait question flagged to the
owner, not picked), learn mode explicit and never default, verify pods on a
fixed operator-owned policy, per-iteration policy hash in history, and
"count unknown" on relay outage.

C1 gets most of the hardening right — no SA token, all caps dropped, no
privilege escalation, seccomp `RuntimeDefault`, read-only rootfs, workspace
and scratch as `emptyDir`. Two defects break the slice's own goals.

---

## P1 — Blocking

### I34. `spec.agent.env` can inject Secrets into the agent container

- [ ] Done

**Where:** `api/v1alpha1/loop_types.go` `AgentConfig.Env []corev1.EnvVar`;
`ensureSandbox` (`Env: loop.Spec.Agent.Env`).

**Problem:** `corev1.EnvVar` supports `valueFrom.secretKeyRef` (and
`configMapKeyRef`, `fieldRef`, `resourceFieldRef`). A Loop author can write

```yaml
agent:
  env:
    - name: GITHUB_TOKEN
      valueFrom: {secretKeyRef: {name: push-token, key: token}}
```

and the kubelet mounts that credential straight into the agent — the exact
thing ADR-0006 exists to prevent. It also lets a Loop author read any Secret
in the namespace through the agent, bypassing whoever owns that Secret.

**Fix:**
1. Red: envtest — creating a Loop whose `agent.env` has any `valueFrom` is
   rejected at admission.
2. Green: replace `[]corev1.EnvVar` with a Coxswain type
   `[]AgentEnvVar{Name, Value string}` (literal values only), or keep the type
   and add CEL `self.all(e, !has(e.valueFrom))`. The custom type is better: the
   API then can't express the unsafe form.
3. Also reject names the platform owns (`COX_*`, e.g. `COX_MODEL_BASE_URL`,
   set by the operator in C2) so a Loop can't point the agent past the proxy.

### I35. `runAsNonRoot` without a UID breaks the default and README sandbox

- [ ] Done

**Where:** `ensureSandbox` — `RunAsNonRoot: true`, no `RunAsUser`/`RunAsGroup`
/ `fsGroup`; default image `docker.io/library/golang:1.26` runs as root.

**Problem — verified on kind 1.34:** a pod with `runAsNonRoot: true` and the
`golang:1.26` image fails with `CreateContainerConfigError: container has
runAsNonRoot and image will run as root`. So after C1 the default sandbox, and
the README "Getting started" flow (step 4 shows the pod `Running`), no longer
start. envtest can't catch this — no kubelet.

**Fix:**
- Set `runAsUser`/`runAsGroup` (e.g. 65532) on the agent container and
  `fsGroup` on the pod so `/workspace` and `/scratch` are writable by that UID.
- Point the platform defaults at the UID: `HOME=/scratch`,
  `TMPDIR=/scratch/tmp` (or mount an `emptyDir` at `/tmp`), because a
  read-only rootfs otherwise breaks every tool that writes `~/.cache` or
  `/tmp` (Go's build cache, git, npm).
- Acceptance: re-run README steps 1–4 on kind — Sandbox `Ready`, pod
  `Running` — and fold that into the I27 e2e so the README can't regress
  silently again.

---

## P2 — Design

### I36. Resource limits are missing

ADR-0006 item 4 requires CPU/memory limits; C1 sets none, so one agent can
starve the node. Add `resources` with platform defaults (e.g. from the
`coxswain-agent-defaults` ConfigMap ADR-0006 mentions) and an optional
override on `spec.agent` bounded by a cluster maximum. Add an
`ephemeral-storage` limit too — `/workspace` and `/scratch` are `emptyDir`
and an agent can fill the node's disk.

### I37. `spec.agent` with no image falls back to a root golang image

With `spec.agent` omitted the sandbox runs `golang:1.26` + `sleep infinity`
— fine as a Phase 0 stand-in, but it's now the hardened, supposedly
agent-agnostic default. Decide the default: the reference runner image once
it exists (A-slices), or require `spec.agent.image` via CEL. Until then,
note the stand-in in the README's status line.

---

## P3 — Cleanup

### I38. Commit hygiene and marker placement

- `ac075c1` changed `AgentConfig` but the deepcopy regeneration landed
  separately in `4a621d8`, so `ac075c1` builds with a stale
  `zz_generated.deepcopy.go` (shallow copy of `Env`). Run `make manifests
  generate` before each API commit so every commit is self-consistent.
- In `AgentConfig`, `+optional` sits at the end of the doc sentence
  (`// … endpoint. +optional`). Markers must be on their own line or
  controller-gen ignores them — and the literal "+optional" text now appears
  in the CRD field descriptions. Move each to its own `// +optional` line and
  regenerate.

---

## Next round will check

- I34, I35 (P1) before C2 builds on the pod spec.
- C2 (model proxy sidecar), C4 (publish step).
