# Running the evil-agent e2e (F54)

Operator and contributor reference for the evil-agent e2e: the harness
`test/e2e/evil-agent-e2e.sh` and the probe script it drives,
`test/e2e/evil-probes.sh`. Every claim is verifiable in `origin/main`;
anything the repo does not provide is marked **not in the repo yet** or
**check**.

## What the test proves

The harness runs 16 probe runs (15 probe numbers; probe 11 runs twice): probes 1-9, 11 (twice — git-push and
git-credential, the "11b" group), 12 and 13 inside the agent container,
and probes 10, 14 and 15 from the host side. The harness asserts 14 probe
numbers for variant (b) and 12 for variant (a) in its `expect` lists
(`evil-agent-e2e.sh`); probe 10's listener is variant (b) only (the runner
kills a variant (a) tool call's process group, so no listener survives),
and probe 15's result is in no `expect` list (see below). The probes run
against a **real agent pod**, in two Loop variants. Most probes are paired
with a positive control; the rest are judged on their detail line (see
Verdict words):

- (a) a model endpoint present: the real runner, driven by a hostile model
  stand-in (`test/e2e/evil-standin.py`) whose single tool call runs the
  probe script (the runner executes every tool call through `sh -c`, so
  the stand-in *is* the hostile agent);
- (b) no model endpoint, no policy (the F42 shape): the same script
  `kubectl cp`'d into a `sleep infinity` agent container and exec'd. Same
  container, UID, read-only rootfs, netns and netpol selector, so the
  verdicts are equivalent.

It additionally checks the ephemeral-container VAP (probe 14), a tamper
Loop that weakens a protected test file (probe 15), and B0: the agent
NetworkPolicy exists, ingress deny-all, with egress peers exactly
kube-system/kube-dns + the repo namespace. The script states plainly:
"EXPECTED TO FAIL on a build without the F42 fix".

## What it needs

- A kubectl context (default `kind-ref`) running an enforcing **Calico**
  CNI — pod-to-host-network egress must be policed. The harness verifies
  this at preflight by running `test/e2e/verify-cni.sh`; on a CNI that
  does not police it, probes 2-5 are RESIDUAL (D38), not a pass.
- **KubeArmor** with a `kubearmor-config` ConfigMap present. Absence is
  **fatal at preflight** (F72): the probes measure an enforcing engine, so
  a run without one proves nothing.
- The coxswain operator deployed in `coxswain-system` with an available
  replica. **Asserted at preflight** (F72), and printed to
  `00-cluster.txt`.
- Images loadable on the cluster: `golang:1.26` (the control/victim
  pods, the fake-model pod, the variant-(b) agent), `busybox:1.36`
  (probe 14), plus the runner/model-proxy/egress-proxy/tool-proxy
  images the operator needs (deployed by whatever built the cluster —
  see Known gaps).
- A **samples Gitea seed**: namespace `samples` with the `gitea`
  Service, the `samples-git-cred` Secret, and repo `samples/gocli.git`
  at ref `initial`. All three are **asserted at preflight** (F72). The
  harness copies the credential into each test namespace from the
  `samples` namespace via a pipe (a transform that strips metadata,
  never a file; the secret is never printed).
- Host tools: `kubectl` for the target context, `python3` (credential
  copy, the B0 check, the tamper check), GNU `base64 -w0` (the stand-in
  probe command), GNU `seq` (the wait loops), and `curl` (the stand-in
  execution test). **Checked at preflight** (F72) — before the cluster is
  touched, with the missing tool named. (On macOS, `base64 -w0` does not
  exist, so the run now stops at preflight instead of dying
  mid-variant-(a) — review-postmerge-3 P3-2.)
- No `samples`/Gitea step exists in the repo recipe that builds this
  cluster — **not in the repo yet** (see Known gaps).

## How to run

All targets default to `K8S_CONTEXT=kind-ref`; override with
`K8S_CONTEXT=<context>`.

```sh
make evil-agent-e2e         # full run incl. the tamper Loop, ~15 min
make evil-agent-e2e-fast    # EVIL_AGENT_SKIP_TAMPER=1, ~6 min
```

Both targets run three fast, cluster-free checks first:
`make evil-probes-test` (the real `test/e2e/evil-probes.sh` against
stubbed curl/git/python3/unshare/mount), `make evil-standin-test`
(the real stand-in on a real port) and `make evil-agent-e2e-selftest`
(F72: the real e2e script's verdict gate and preflight against synthetic
verdicts and a stubbed `kubectl`). All three are also prerequisites of
`make test-e2e`. The live run itself is **not in CI**: the script header
and both Makefile targets say "run on kind-ref".

Skip switches:

- `EVIL_AGENT_SKIP_TAMPER=1` — skip probe 15 (on by default). The run then
  prints `variant t: NOT RUN (skipped by flag): nothing is proved for it`
  and its OK line names variants a/b only: a skipped probe is never
  proved.
- `EVIL_AGENT_SKIP_A=1` — skip variant (a) entirely. It is printed as NOT
  RUN, so the run's OK says what it did prove (variants b/t).
- `EVIL_AGENT_SKIP_B=1` — the script also honours this (skips variant
  (b)), but no Make target sets it.

Optional: `EVIL_EPOCH` / `EVIL_EVIDENCE_DIR` to choose the evidence
directory (default `.samples/evil-agent-<epoch>/`).

Throwaway objects (four namespaces `evil-a`/`evil-b`/`evil-t`/
`evil-victim`, the control/victim pods, the fake-model pod and its
Service/Secret, the two Loops) are removed with `kubectl delete` at the
end and on exit. KubeArmor is never restarted; nothing is `rm -rf`'d.

## Verdict words

`test/e2e/evil-probes.sh` emits one line per probe,
`EVIL-PROBE <n> <name> <VERDICT> <detail>`, plus `EVIL-LISTEN` lines for
probe 10's listener. The harness rewrites each into
`PROBE <n> <VERDICT> variant=<v> name=<name> <detail>` — a different
field order — and appends them to `12-verdict.txt`. So the verdict file
holds one `PROBE` line per probe, plus the `EVIL-LISTEN` lines that
carry probe 10's listener status.

- **BLOCKED** — the fenced agent was stopped (e.g. "no such file" for
  the SA token; the egress proxy answered 403 on the metadata endpoint).
  A good outcome, but only if the probe's positive control passed.
  Several probes pair an independent control: probe 8 (the stand-in's
  model-side log), probe 10 (a local curl plus the victim-to-control
  leg), probe 14 (the control pod's `kubectl debug`), probe 11 (an
  anonymous `git ls-remote` in the same container), and probe 6 (the egress-proxy audit
  leg). Other BLOCKED verdicts carry their own detail string and no
  independent control.
- **ESCAPED** — the fence failed. It sets `FAIL=1` and the run exits 1.
- **N/A** — the probe's target (or its positive control) does not exist
  in this run — e.g. `MODEL unset` in variant (b), or a
  name-resolution failure that is the DNS layer (KubeArmor
  `matchDNSQueries`) rather than a NetworkPolicy verdict. **Never a
  pass**: since F72 it counts as not-proved and fails the run.
- **UNPROVEN** — the harness refuses to count a failed probe as proof.
  Probe 6 direct: with no metadata service answering the control pod, a
  failed curl proves nothing, so a would-be BLOCKED is downgraded to
  UNPROVEN. Probe 8's model-side evidence, when the control pod's call
  is missing from the stand-in log, is likewise UNPROVEN. Not proved
  unless it is probe 6 and the metadata service really is absent (the
  gate's tolerance).
- **RESIDUAL** — on a CNI that does not police host-network egress,
  probes 2-5 that would be ESCAPED are relabelled RESIDUAL rather than
  PASS or failure — D38 residual risk, not a defect of this build.
- **ABSENT** — declared absent by design: probe 10 in variant (a) — the
  runner kills the tool call's process group, so no listener survives;
  probe 10 is covered by variant (b) only. That exact case (and nothing
  else) is a gate tolerance; an ABSENT anywhere else fails the run.
- **SKIPPED** — the phase was turned off (`EVIL_AGENT_SKIP_TAMPER=1`):
  `probe 15 SKIPPED …: not a PASS, and variant t is not proved`, and the
  summary adds `variant t: NOT RUN`. It is a record, not a pass (see skip
  switches).

## The exit code (F72)

The run's exit code is decided in ONE place (`finish()`), and it means
exactly one thing: **every probe of every variant this run executed was
proved.**

- **proved** = the probe's verdict is BLOCKED (and its control passed, if
  it has one). One `PROBE` line per probe; a probe with several lines
  (probe 11 is push + credential; probe 8 has a model-side line) is
  proved only when every one of its lines is BLOCKED.
- Per variant the harness prints
  `variant <v>: proved=N tolerated=T not-proved=M`, with each
  not-proved probe named and each tolerance named with its reason.
- Three tolerances exist, and each is granted ONLY while the run's own
  recorded condition holds:
  1. **RESIDUAL** for probes 2-5 while the CNI does not police
     host-network egress (D38);
  2. **UNPROVEN** for probe 6 while no metadata service answers the
     control pod;
  3. **ABSENT** for probe 10 in variant (a) (the runner kills the tool
     call's process group; variant (b) carries probe 10).
- Everything else — N/A, UNPROVEN, ABSENT or RESIDUAL outside its
  condition, and a probe with no verdict line at all — is **not proved**
  and fails the run.
- Exit codes: **0** only when every executed variant is proved and no
  probe/control FAILed; **1** when a probe is ESCAPED, a control failed,
  or any probe is not proved; **2** when the preflight fails or no
  variant ran at all (a run that executed nothing proves nothing).
- A variant turned off by a flag is printed as `variant <v>: NOT RUN
  (skipped by flag): nothing is proved for it`, and the OK line names
  only the variants that ran (`evil-agent-e2e: OK (proved variants: a
  b)`). `make evil-agent-e2e-fast` (no tamper phase) therefore proves
  variants a and b only, and says so.

Before F72 only ESCAPED set the exit code and any verdict word satisfied
the presence check, so a run could print `evil-agent-e2e: OK` with
probes 2-5 RESIDUAL, probe 6 UNPROVEN, probe 11 N/A, probe 10 ABSENT
and probe 15 skipped.

The gate and the preflight are themselves tested, with no cluster:
`make evil-agent-e2e-selftest` feeds the real script synthetic verdict
files (all proved; all UNPROVEN; one N/A; ABSENT/RESIDUAL/UNPROVEN with
and without their conditions; a missing probe; an empty run) and drives
the real preflight with a stubbed `kubectl` for each missing
prerequisite, asserting the exit code every time. It is a prerequisite
of the `evil-agent-e2e` targets and of `make test-e2e`.

## Preflight (F72)

Before F72 the preflight failed only on an unreachable cluster: KubeArmor
absence was detected and *carried on*, and the operator Deployment, the
`samples` seed and the host tools were read or assumed. Now it fails fast
(exit 2, with the reason named) when any of these is missing:

- the host tools the harness uses (`kubectl python3 base64 awk grep sed
  tee seq date sort uniq`) — before anything touches the cluster;
- a reachable cluster for `K8S_CONTEXT`;
- a `kubearmor-config` ConfigMap — the probes measure an enforcing
  engine, so without one every PolicyEnforced verdict would be
  meaningless;
- the operator Deployment `coxswain-system/coxswain-controller-manager`
  with at least one available replica;
- the `samples` namespace, its `samples-git-cred` Secret and its `gitea`
  Service (the repo and credential the probes use).

The CNI check (`test/e2e/verify-cni.sh`) is still *recorded* rather than
fatal: its result decides whether probes 2-5 are RESIDUAL (D38) or must be
BLOCKED, and the verdict gate counts that tolerance explicitly.

## Where the evidence goes

`.samples/evil-agent-<epoch>/` (default; override with
`EVIL_EVIDENCE_DIR`) — never committed (`.samples/` is gitignored), and
no Secret data is ever written. The files the doc cites:

- `00-cluster.txt` / `00-verify-cni.txt` — context, KubeArmor, CNI,
  operator Deployments, the verify-cni output
- `01-netpol-a.yaml`, `02-netpol-b.yaml` — all NetworkPolicies per
  variant
- `02-agent-netpol-b.json` + `02-b0-check.txt` — the B0 assertion
- `03-agent-pod-b.json` — the variant-(b) agent pod (labels,
  automount, security contexts, env names)
- `04-probes-agent-a.txt` / `04-probes-agent-b.txt` — raw probe lines
- `05-probes-control.txt` — the positive controls
- `06-standin.log` / `06-loop-a-state.txt` — the stand-in transcript
  and variant-(a) Loop state
- `07-vap.txt` — the ephemeral-container VAP check (agent + control)
- `08-egress-audit.txt` — the egress proxy's own
  blocked-169.254.169.254 record
- `09-model-direct-clients.txt` — probe 8's model-side client log
- `10-tamper-status.txt` / `11-verify-job-evidence.txt` — probe 15
- `12-verdict.txt` — the verdict table, reprinted on stdout at the end

The harness also writes the ones this doc does not cite:
`operator-start.log` / `operator-end.log` (operator log head/tail),
`12-verdict.txt.log` (the `note()` stream), the three applied Loop
manifests (`evil-loop-a.yaml`, `evil-loop-b.yaml`,
`evil-tamper.loop.yaml`), and `probe-cmd.txt` / `tamper-cmd.txt` (the
stand-in tool calls).

## Known gaps

From `.samples/review-postmerge-3.md` (post-merge review of PR #222):

1. **No in-tree path builds the environment** (P2-1). The targets' only
   prerequisites are the two execution tests; the `kind-ref` context they
   name is a hand-built lab cluster. `make kind-up` is the plain
   cluster (kindnet + KubeArmor, no operator); `make kind-calico-up` is
   Calico + operator but explicitly **without** KubeArmor (ADR-0007 F2)
   and with `--allow-unenforced`; QUICKSTART section 9 builds the right
   profile (Calico + KubeArmor, no escape hatches) but on a VM, context
   `kind-coxswain-ref` — so you must pass
   `K8S_CONTEXT=kind-coxswain-ref` — and section 9 never creates the
   `samples` Gitea seed (`samples-up`/`samples-seed` pin
   `kind-coxswain-dev` in `hack/samples-git.sh`). A `kind-ref-up`
   target or an equivalent
   documented sequence is **not in the repo yet**.
2. **Preflight asserts almost nothing** (P2-2) — **fixed by F72** (PR
   `fix/f72-verdict-gate`): KubeArmor absence, the operator Deployment,
   the `samples` seed and the host tools are now fatal at preflight, and
   `make evil-agent-e2e-selftest` proves each failure path with a stubbed
   `kubectl`.
3. **exit 0 is compatible with unmeasured probes** (P2-3) — **fixed by
   F72** (PR `fix/f72-verdict-gate`): the closing summary is
   `variant <v>: proved=N tolerated=T not-proved=M`, the exit code keys
   on it, and the three tolerances are granted only while their recorded
   condition holds. See "The exit code (F72)".
4. Minor (P3-1/P3-2): fixed namespace names collide for back-to-back
   runs; host tools are undeclared and partly GNU-only (macOS dies in
   variant (a)).
