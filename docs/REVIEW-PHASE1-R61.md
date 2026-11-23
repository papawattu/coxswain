# Phase 1 review, round 61: D144 answered (a), I274 closed, and the first kind run of fence-wait

**Scope:** main `bed9ba7` (R60's main) → `3aff005`, 11-20 to 11-22, and the lab run R4 on kind-ref (11-22, 21:21Z–21:28Z) at `3aff005`. Evidence is reviewer-local and not committed: `.samples/r4/run1/`. R57's I271 and I272 are not re-checked here and stay open.

Merged in this window:

| PR | What | Squash | Review on the PR |
|---|---|---|---|
| #510 | R60 (owner merge, tag `review/phase1-r60`) | `3aff005` | pi3, OK + notes |
| #511 | Leader-election timings: lease 60 s, renew 40 s, retry 5 s, and three flags | `1049aff` | none |
| #512 | Release notes v0.3.1 | `61bcacd` | none |
| #513 | fence-wait in the CNI probe's policy pass | `4121e52` | none |
| #514 | Release notes v0.3.2 | `c1da2f1` | none |
| #515 | fence-wait as the sandbox pod's first init container; `SandboxFenceNotReady` | `e142030` | none |
| #516, #517, #520 | The Phase 9 adoption plan and its decisions | `582f396`, `3542634`, `9d5b58e` | none |
| #521 | fence-wait in every proxy pod (model, Judge, egress, tool) | `0d8814c` | none |

v0.3.1 was tagged on `61bcacd` (11-20) and v0.3.2 on `c1da2f1` (11-21). Both releases' notes say "Not proven on kind" (`docs/RELEASE-NOTES-v0.3.1.md:5`, `docs/RELEASE-NOTES-v0.3.2.md:5`).

## Answered

- **D144 = (a)** (owner, 11-22). The kind evidence stands for I274's edit and commit items, and the Manual-reject spec is dropped. The ToolTrace capture stays owed.
- **D145 = (a)** (owner, 11-22, while this PR was open; the question is below). A behaviour PR from another session is reviewed on the PR before it merges. #522 (fence-wait in the Job pods) is the first one.

## Closed

- **I274** is ticked in R59 (`bed9ba7`). D144 = (a) accepts R3 run 2 for the edit and commit items. The gate spec and its mutation are in #509.
- **I275** (the reviewer's lab-script slips) is closed. R4 had one slip of the same kind, caught before the run. My first draft of `r4-lab.sh`, which was not kept, listed the Loop's pods with `-l coxswain.io/loop=$LP`. The proxies do not carry that label (`internal/controller/loop_controller.go:4075`), so it would have logged no proxy fence-wait. Before the run I replaced it with the name-prefix match the script has now (`get pods … | grep "^$LP-"`). The run logged `r4-fence-proxy` (`fence-wait-logs.txt:1`). No slip reached a verdict.

## R4: kind-ref on main `3aff005`

The lab now runs main `3aff005`:
- all five images, CRDs, RBAC and admission (`crd.log`, `admission.log`);
- args moved from `:bed9ba7` to `:3aff005`, otherwise as before (`deploy-after.yaml:48-53,62`);
- the operator log has 31,569 lines and `forbidden` = 0 (`r4-lab.log:46`). The positive control is the one `CNI probe result changed` line.

This is the first kind run of #513, #515 and #521. kind-ref runs Calico.

- **Probe (#513).** The policy pod's fence-wait exited 0 with `FENCE_UP elapsed=1.10 attempts=2`. The control pod has no fence-wait (`fence-summary.txt:2-3`). The new operator's first result was `"old": "Unknown", "new": "CNIEnforced"` at 21:23:13, 34 s after the upgrade (`r4-lab.log:24`).
- **Sandbox (#515).** Each of the Loop's three sandbox pods (Planning, Implementing, and the one after Verifying) has `fence-wait` as `initContainers[0]`, ahead of `init-workspace` in the first pod. Each exited 0 with `FENCE_UP elapsed=1.10 attempts=2`, read from `samples-podwatch.json`. Its security context is uid/gid 65532, a read-only root filesystem, `drop: [ALL]` and RuntimeDefault seccomp (`sandbox-pod.json`). `SandboxFenceNotReady` was never set (`poll.log`). The Loop went Planning → Implementing → Verifying → Succeeded, 21:23:18 → 21:26:34.
- **Model proxy (#521).** `r4-fence-proxy` has fence-wait as its only init container. It exited 0 with `FENCE_UP` (`fence-wait-logs.txt:1-2`).
- **Controls.** Both use the sandbox's real fence-wait container, copied from the pod spec the script saved while the first sandbox pod ran (`sandbox-pod.json`). Only the budget was changed, to 10 s. They ran at 21:26:34, before cleanup deleted the Loop at 21:27:44 (`r4-lab.log:35,47`). Both were pinned to the sandbox's node.
  - **C0, no NetworkPolicy:** exit 2, `FENCE_TIMEOUT elapsed=10.03 attempts=101` (`controls.txt:1`). The kubelet is reachable from a pod with no policy, so fence-wait does not pass when nothing fences the pod.
  - **C1, a deny-all egress policy:** exit 0, `FENCE_UP elapsed=1.10 attempts=2` (`controls.txt:2`).
- **What the timings show.** Every pass is two 0.5 s connect timeouts in a row, with no successful connect before them. So on kind-ref's Calico, the fence was in place before fence-wait's first connect. R4 shows no k3s-style window here. It also cannot measure one smaller than fence-wait's first attempt.
- **Not exercised by R4:**
  - the egress, tool and Judge proxies (the sample has no egress or tool policy and no Judge);
  - the fence-wait timeout path on a real sandbox;
  - a KubeArmor exec allowlist (kind-ref has no AgentPolicy);
  - the verify Job pod, which has no fence-wait (`fence-summary.txt:8`; #521 names it as a later PR).

## P2

- [x] **I276: four behaviour PRs and two releases went to main with no review on the PR** (`7ab11ec`)

  **Where:** #511, #513, #515, #521; tags v0.3.1 and v0.3.2.

  **Problem:**
  - None of the four has a review or a review thread (`gh api …/pulls/<n>/reviews` is `[]` for each). AGENTS.md:24 says the reviewer reviews code on the PR, and AGENTS.md:281 says a behaviour PR does not merge until its evidence exists.
  - #513's and #515's bodies each say the kind run is still owed. #515 changes the sandbox pod, which the PR protocol says needs a kind run.
  - v0.3.2 shipped #513 the day before any kind run. Its notes say so plainly, so nothing is hidden. But the tag went out without the evidence the protocol asks for.
  - The PRs cite a homelab brief (homelab PR #149) and were made in a session I did not brief. Who decided to merge them is not in the repo, and I do not state it.

  **Fix:**
  - R4 is the post-merge kind run for the probe, the sandbox and the model proxy. pi3's read-only audit is the post-merge code review. Its findings, as I re-graded them, are I278–I280 below.
  - The owner decides whether PRs from other sessions go through this review (D145).

  **Acceptance:** the next fence-wait PR, #522, is reviewed on the PR before it merges (D145 = (a)).

- [ ] **I277: the Phase 9 plan reuses decision IDs D140–D143**

  **Where:** `docs/PLAN-PHASE9-ADOPTION.md:311-314`, plus 10 references in that file.

  **Problem:**
  - These four IDs already had meanings before the plan used them:
    - D140 = lessons from a flagged Loop (`docs/PLAN-PHASE8.md:146`);
    - D141 = the Judge proxy only lives while judging (`docs/OPERATOR.md:814`);
    - D142 = a new leader before its first probe (`docs/REVIEW-PHASE1-R58.md:70`);
    - D143 = the read-only Planning workspace (`docs/REVIEW-PHASE1-R59.md:43`).
  - Code comments and docs cite the old meanings, for example "LeaderElectionReleaseOnCancel (D142a)" in #511's body and `docs/QUICKSTART.md:102`.
  - AGENTS.md:48 says IDs are unique across rounds.

  **Fix:** renumber the Phase 9 decisions to D146–D149 in `PLAN-PHASE9-ADOPTION.md`. This is a docs-only PR.

  **Acceptance:** no Phase 9 decision text appears under an ID that an earlier doc defines. Show it with `git grep -n '\bD14[0-3]\b' docs/PLAN-PHASE9-ADOPTION.md` printing nothing, and the same grep over `docs/PLAN-PHASE8.md` still matching as the positive control.

## P3

pi3 reported P1=1, P2=3, P3=3. I re-checked each one. Two of its P2s say "verified, no defect" in their own bodies, and I drop them. Its P1 is a wrong diagnostic message on a path that fails closed, so I file it as P3 (I278). Its P3-2 and P3-3 record checks that passed.

- [ ] **I278: the probe says "still reachable" for every fence-wait timeout**

  **Where:** `internal/cni/pod_prober.go:346-353`.

  **Problem:**
  - Any exit 2 from the policy pass becomes `CNIUnenforced` with the detail `KUBELET_NODE (still reachable after the … fence-wait budget)`.
  - The script also exits 2 when the streak is reset for the whole budget by an error it does not count, such as EPERM from an LSM or ENETUNREACH (`internal/cni/fence_wait.go:162-171`). In that case the kubelet was never reached.
  - I re-ran the real script, extracted from `fence_wait.go` with only `socket` stubbed and a 0.5 s budget:
    - every connect EPERM: `fence-wait: ignoring connect error [Errno 1] Operation not permitted (not a fence)`, then `FENCE_TIMEOUT elapsed=0.50 attempts=6`, exit 2;
    - every connect succeeds: `FENCE_TIMEOUT elapsed=0.50 attempts=6`, exit 2, the same line;
    - every connect refused, the control: `FENCE_UP elapsed=0.10 attempts=2`, exit 0.
  - The verdict fails closed: Loops are held. Only the detail is wrong.

  **Fix:** the script counts successful connects and reports them in the `FENCE_TIMEOUT` line. The probe says "still reachable" only when the last attempts connected; otherwise it says what the script saw.

  **Acceptance:** a probe spec for the EPERM case asserts the detail does not say "reachable", and it FAILS on today's code.

- [ ] **I279: `SandboxFenceNotReady` is not refreshed while a Loop is Paused, Succeeded or Failed**

  **Where:** `internal/controller/loop_s4_phase.go:1169`. That is its only call site, and it is reached only after the early returns at `:700-705`.

  **Problem:**
  - A Loop that goes Paused or terminal with the condition True keeps it True after the pod is gone.
  - That contradicts #515's body: "It clears (FenceUp) when … the pod is gone".
  - `SandboxNotStarting` has the same placement (`:1166`), so this pattern predates #515.

  **Fix:** clear both conditions on the Paused and terminal paths, or document that they are frozen there. The owner of the next fence-wait PR chooses.

  **Acceptance:** an envtest spec: condition True, Loop suspended, pod gone, reconcile; then assert the chosen behaviour. The spec fails on today's code if the choice is "clear".

- [ ] **I280: `--cni-fence-wait-budget`'s startup check has no spec, and v0.3.1's notes understate its kind proof**

  **Where:** `cmd/main.go:312-316`; `docs/RELEASE-NOTES-v0.3.1.md:5`.

  **Problem:**
  - `grep -rn 'fence-wait-budget' cmd/*_test.go` gives 0 matches. The leader-election flags have a spec: `leader-elect-lease-duration` gives 1 match, the control.
  - v0.3.1's notes say each entry was "covered only by its PR's unit tests and CI". But I273 and I274 were proven on kind by R2d and R3 (R60), on candidate `badd801`, whose tree is the same as `bed9ba7`. #511 was not proven on kind.

  **Fix:**
  - Move the budget check into a function with a spec, as `leTimings.validate()` already does.
  - Correct the v0.3.1 notice to name R60's runs for I273 and I274.

  **Acceptance:** the spec rejects a zero budget and a budget equal to `--cni-probe-timeout`, and it FAILS with the check removed. The notice names R60.

## Owner decision

- **D145: do PRs from other sessions (such as the homelab session's fence-wait PRs) go through this review before merge?**
  - **(a) Recommended: yes, for any PR that changes behaviour.** The protocol's rules are about the change, not who wrote it. The homelab session can open the PR, I review it on the PR, and you merge on the verdict. Docs-only PRs, like the release notes and the plan, can merge without review, as now.
  - **(b) No: other sessions merge on their own evidence, and I audit after the merge in the next round,** as R61 does here. That is faster. But a defect like I278 reaches a release before anyone reads the code.

## What I did not verify

- **fence-wait on k3s.** The PRs' homelab timings (0.20–1.21 s) are the authors' numbers. I did not re-run them.
- **The egress, tool and Judge proxies' fence-wait on kind.** R4 had only the model proxy. pi3 read their NetworkPolicies and found that each denies a private `NODE_IP:10250`. Only the model proxy is shown by a run.
- **fence-wait with a KubeArmor exec allowlist.** #515's body says python3 would be denied, so fence-wait would crash-loop and the Loop would wedge, failing closed. Not tried.
- **#511's timings under a real API stall.** pi3 checked that `validate()` covers client-go's constraints. Nothing here stalled the API.
