# Phase 1 review, round 26: the alpha releases (A4 to A6)

Round 26, 2026-10-12, since tag `review/phase1-r25`. It covers the second half
of the alpha phase (`docs/PLAN-ALPHA.md`): the self-explaining PR (A4),
packaging and release (A5), and alpha acceptance (A6). Four alpha releases
were cut; A6 found three release bugs and a credential leak, each fixed below.

| PR | Item | Merge |
|---|---|---|
| #119 | A5 release workflow (D56): a `v*` tag publishes ghcr images + `install.yaml` | d5125c4 |
| #120 | A4 self-explaining deliver PR description | 571b8be |
| #121 | D55 `hack/github-secret.sh` | 30acf61 |
| #122 | A5 `install.yaml` tags every released image; `docs/QUICKSTART.md` | ab538d1 |
| #123 | R25 I81: delivery branch `coxswain/<loop>-<uid8>` | 213dacf |
| #124 | The A3 e2e stall in Implementing: vLLM backends, environmental | 4653e9a |
| #125 | A6 alpha acceptance (`make alpha-e2e`) | d640771 |
| #127 | configmaps RBAC: `list;watch` (I86) | b72ece1 |
| #128 | D55: git-cred applied server-side (I87) | cdceff0 |
| #129 | Watch the Loop's KubeArmorPolicy, CRD-gated (I88) | 1ac6879 |
| #130 | I85: the admission binding ships in `install.yaml` | 41635e9 |
| #131 | e2e: wait for the kill Loop's delivery | 327945f |

**Releases:** `v0.1.0-alpha.1` (A5), `alpha.2` (I86), `alpha.3` (I87, I88),
`alpha.4` (I85). Each release run built and pushed the five
`ghcr.io/papawattu/coxswain-*` images and attached `install.yaml`.

**Status:**
- **A4 done.** The PR description has a facts section from spec/status only,
  per-iteration verify results and budget, plus the agent's plan fenced and
  marked untrusted. The JSON payload is built with `json.dumps`, which closes
  the earlier `printf` injection path.
- **A5 done.** `install.yaml` pins all five images to the tag and keeps the
  base manager args. The QUICKSTART follows the release.
- **A6 done**, on `v0.1.0-alpha.3`, on the existing coxswain-alpha (kindnet +
  KubeArmor):
  - the real model (`qwen3.8-27b`);
  - the main Loop's own first-time draft PR, papawattu/coxswain-sandbox#19;
  - the held case on an active Loop with no Loop touch;
  - the kill case resuming on the same PVC, with its delivery,
    papawattu/coxswain-sandbox#21 (#131);
  - exactly one PR.
- **alpha.4** carries the admission gate. A smoke test on coxswain-alpha
  shows `kubectl debug` denied by `coxswain-deny-ephemeral-containers`.
- **The network gate was not exercised:** A6 ran with
  `--allow-unenforced-network`, labelled as the escape hatch. See Q2.

**Owner decisions this round:**
- **D55** (recorded in R25) stands. #128 tightens it: no
  `last-applied-configuration` copy of the token.
- **D56: releases come from a tag-triggered workflow** using `GITHUB_TOKEN`
  (`packages: write`), with no personal ghcr token. The owner pushed the
  workflow once (#119) and made the packages public.
- **A4 merged without its live GitHub run;** the run moved to A6, which
  produced papawattu/coxswain-sandbox#19.

---

## P1 (all fixed this round)

### I85. The release shipped no admission binding

- [x] Fixed — 41635e9

**Where:** `config/default/kustomization.yaml` included `../admission`.

**Problem:** config/default stamps `namespace: coxswain-system` and
`namePrefix: coxswain-` on everything it includes. That put a namespace on the
cluster-scoped ValidatingAdmissionPolicy and broke its binding, so alpha.1–.3
shipped the deny-ephemeral-containers policy with no working binding: the gate
was off in every release. (The reviewer first blamed a `----` separator; that
was a misread of grep's `-51-` context prefix. The builder found the real
cause.)

**Fix:** build `config/admission` separately (VAP cluster-scoped, binding
namespaced) and append it in `hack/build-installer.sh` and the deploy targets.

**Acceptance:**
- `hack/build-installer-test.sh` asserts both objects, with `policyName`
  matching.
- Deleting the admission build fails the test.
- The alpha.4 smoke shows the denial.

### I86. A fresh install wedged on ConfigMap RBAC

- [x] Fixed — b72ece1

**Problem:** A4 added a ConfigMap read through the manager cache, but the
marker granted `get;create;update;delete`, without `list;watch`. A fresh
alpha.1 install wedged with `cannot list resource configmaps at the cluster
scope`. The A4 review accepted "the cache isn't label-filtered" without
checking that the cache needs list/watch.

**Fix:** `list;watch` on the marker. `rbac_coverage_test.go` asserts
`get;list;watch` for every type read through the cache; dropping `list` fails
it.

### I87. The A6 e2e wrote the owner's GitHub token to disk

- [x] Fixed — d640771 (script), cdceff0 (helper)

**Problem:**
- `alpha-e2e.sh` saved `kubectl get secret git-cred -o json` into its evidence
  folder on every run. That is the D55 token, base64, mode 644, in 13 runs.
- The client-side apply in `hack/github-secret.sh` also kept a second copy in
  the `last-applied-configuration` annotation.
- `.samples/` is gitignored and was never committed. The reviewer shredded
  all 13 files.

**Fix:**
- the script records only the Secret's name, type and key names, and fails if
  any evidence file contains the token;
- `github-secret.sh` uses `kubectl apply --server-side`, with a test that
  fails on a client-side apply;
- live check: no annotations.

**Owner:** rotating the token is a precaution; the exposure was local only.

### I88. A tampered KubeArmorPolicy went unnoticed until an unrelated reconcile

- [x] Fixed — 1ac6879

**Problem:** the operator didn't watch KubeArmorPolicy, so A1's spec-hash
check only re-ran when something else touched the Loop. The first A6 held case
"passed" only because the script nudged the Loop with an annotation.

**Fix:**
- watch the Loop's KubeArmorPolicy (`coxswain-<loop>`), registered only when
  the RESTMapper resolves the CRD. `install.yaml` doesn't ship it: KubeArmor
  does;
- read-before-write ordering, so a tamper is held as `KaptTampered` instead of
  being silently repaired;
- a no-CRD envtest; making the watch unconditional fails it;
- A6 alpha.3: tamper in Planning with no Loop touch gives `KaptTampered` at
  t=0s, then the gate holds and recovers.

## P2

### I89. The agent waits 10 minutes on a hung model call

- [ ] Open

**Where:** the runner's model client.

**Problem:** in #124, the vLLM backends went silent for about 24 minutes. Each
runner call hung for its full 10-minute timeout before the runner exited and
was back-off restarted. That makes a Loop's progress hostage to one stuck
request.

**Fix:** a shorter per-request timeout with bounded retry and backoff, and a
Stalled/ModelUnavailable reason surfaced on the Loop.

**Acceptance:** a test with a model stub that hangs: the runner times out
quickly, retries, and the Loop reports the reason.

## P3: process

### I90. Builder habits seen this round

- [ ] Open (AGENTS.md; partly R25 I84)

- **Causes stated without evidence:**
  - "flaky agent" (#123);
  - "vLLM unreachable from a fresh cluster by construction" (#125, first
    attempt; it was reachable);
  - "would have completed if given time" (#130; #131 then showed it).
- **Fallbacks that hide failures:** the A4 ConfigMap ensure was made
  best-effort, which turned a missing permission into a silent one-line PR
  body.
- **Work around the release instead of reporting it:** the first A6 runs added
  RBAC by hand and patched the admission binding in.
- **Branch scope:** the I86 fix was first cut from the A6 branch (#126,
  closed) and would have landed the unreviewed e2e.
- **`pkill -f`** against AGENTS.md's kill-by-PID rule.
- **"lint clean"** stated while CI failed (#129).

**Fix:** AGENTS.md:
- a release defect found by an e2e is reported and fixed in the release, never
  worked around in the script;
- a fallback must surface its failure (Event or condition) before it degrades;
- every fix branch starts from `main`;
- plus R25 I84's rules.

---

## Questions for the owner

### Q2. The supported alpha topology for the network gate

A6 proved the KubeArmor policy gate but ran the D38 network gate with the
`--allow-unenforced-network` escape hatch. The network gate needs an enforcing
CNI (Calico), and the D38 Calico profile deliberately omits KubeArmor: ADR-0007
F2 warns that restarting a KubeArmor agent can wedge the shared kernel's BPF,
and this host already runs two.

**Options:**
- (a) Prove Calico + KubeArmor together on a host without another KubeArmor.
- (b) Ship the alpha as KubeArmor-only, documenting that the network gate
  needs an enforcing CNI (as D50 did for the DNS gap).

## Carried

- R25 I82, I83, I84: open as written. I82 and I84 are with the builder.
- R22 I59 (design) and I62 (workflow edits are an owner push): open.

## Next

1. I82 and I84 (in progress), then I89 and I90.
2. Q2 at the owner's direction.
