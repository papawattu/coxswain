# Phase 1 review, round 63: I281 closed and proven on kind, and two gaps in the activity audit

**Scope:** main `7fbdbda` (R62's main) → `76b5d4d`, 11-23, and the lab run R5 on kind-ref (Calico), with two upgrades. Evidence is reviewer-local and not committed: `.samples/r5/hold45/` and `.samples/r5/hold45b/`, made by the script `.samples/r5/r5-lab.sh`. R57's I271 and I272, and D144's ToolTrace capture, stay open.

Merged in this window:

| PR | What | Squash | Review on the PR |
|---|---|---|---|
| #524 | R62 (owner merge, tag `review/phase1-r62`) | `7fbdbda` | pi3, after the merge (below) |
| #525 | I281: the CNI probe waits for stale probe pods to go, and retries an unavailable result on a backoff | `76b5d4d` | reviewer: CHANGES, CHANGES, then OK |

## Closed

- **I281** is ticked in R62 (`76b5d4d`). Its three acceptances:
  - The spec where a finalizer holds the stale pod: `TestProbeWaitsForStalePodWithFinalizer`. M4 (the wait's Get through the cached client) FAILS it.
  - The backoff specs: `TestProbeUnavailableRetriesOnBackoff` and `TestBackoffResetsAfterGoodResult`. M3 (no reset) FAILS the second in 20 of 20 runs.
  - The kind upgrade, R5 below.

## #524 merged before its review, and pi3's review after the merge

- I sent pi3 `/new` before #524's brief. The `/new` asked for a confirmation, the clear was cancelled, and pi3 never read the brief. I did not check the pane, and the owner merged #524 on my report that the review was underway. So R62 went in with only my own checks behind it. From now on I check that pi3's pane shows the brief being worked before I say a review is running.
- pi3 then reviewed `7fbdbda` after the merge: P1 = 0, P2 = 0, P3 = 1.
  - **Its P3 is dropped.** It read I282 as saying the tree went back to `main` between 11-20 and 11-22. R62 says "the tree stayed detached until the next entry", and its "then back to `main`" is the 11:04:27 entry, which is right.
  - **One of its supporting facts was wrong.** pi3 said `bed9ba7` was main's tip from 11-20 to 11-22. `git log -1 --format=%cI` gives `1049aff` at 2026-11-20T21:01:16 and `c1da2f1` at 2026-11-21T20:28:32, both after `bed9ba7`. Its conclusion was still right.

## #525's review, and R5 on kind

The unit specs alone would have passed a fix that does nothing on a real cluster.
- **Round 1, at `1191959`:**
  - After any good result the backoff did not restart. A scratch test (enforced, unavailable, enforced; `Interval` 400 ms, `RetryInitial` 20 ms) showed a 400.9 ms gap where the retry should have been about 20 ms.
  - S1 stubbed out the create, so it PASSed with the wait removed.
- **Round 2, at `c1ba5c7`, found by R5 run A:**
  - `waitProbePodsGone` read through `p.cfg.Client`, the manager's cache. Its Pod cache holds only proxy pods (`cmd/main.go:410-411`), so every probe pod reads NotFound and the wait does nothing. The prober's own comment says Gets must use the direct reader (`internal/cni/pod_prober.go:204-210`).
  - The specs set `Client` and `Reader` to the same fake, so they could not see it.
- **R5:** each run upgrades the lab from main `7ab11ec` to the candidate. Just before the upgrade, a `coxswain-cni-probe-control` pod is created, held by the finalizer `coxswain.io/r5-hold`, and deleted. The finalizer is removed 45 s later. Then one Loop runs, and main is restored.

| | Run A, `c1ba5c7` | Run A2, `fa9d121` (= #525's head) |
|---|---|---|
| Hold | 20:50:32 → 20:51:17 | 21:11:45 → 21:12:30 |
| First probe result | 20:50:34 `ProbeUnavailable … object is being deleted … already exists` | 21:13:01 `CNIEnforced` |
| Recovery | 20:51:52 `CNIEnforced`, 78 s after the unavailable result (on main in R62's run, the same gap was 10 min 32 s: 21:56:58 → 22:07:30) | not needed; the new control pod was created at 21:12:31, 1 s after the release (`probe-podwatch.json`) |
| Loop | Succeeded | Succeeded |
| `forbidden` (positive control) | 0 (2) | 0 (1) |

- **Lab state:** both runs end with `restored args == old: yes` and 0 Loops. The lab runs `7ab11ec`. That is not main `76b5d4d`, so the lab does not yet have #525.
- **Not exercised on kind:** a stale probe pod that never goes away (S2 covers it), and k3s.

## P2

- [ ] **I284: two routes out of the sandbox bypass the egress proxy, so the activity audit has no per-connection record of them**

  **Where:** `internal/controller/loop_controller.go:5244-5247` (DNS) and `:5260-5272` (the repo host); `docs/adr/0007-agentpolicy-default-deny-ebpf-audit.md:649-651`.

  **Problem:**
  - ADR-0007 says: "every external connection the agent makes goes through the egress proxy, so every connection is audited" (`:649-651`).
  - The sandbox pod's NetworkPolicy also allows:
    - port 53 to kube-dns, for every Loop (`:5244-5247`);
    - the workspace repo's host and port, when the repo is a cluster Service (`:5265-5272`).
  - Neither goes through the egress proxy. The repo rule is pod-wide, so the agent container can reach the git server, not only the clone init container. The code records this as an accepted MVP limitation (`:5260-5264`), but the ADR still says every connection is audited.
  - KubeArmor recorded one socket-level connect to each in run A2 (`.samples/r5/hold45b/operator.stream.log`):
    - `sin_port=53 sin_addr=10.96.0.10`;
    - `sin_port=3000 sin_addr=10.96.241.68`.
    Both are at 21:13:25, when the Planning pod started, so they are most likely the clone. Neither record carries what was sent: not the DNS names, not the git traffic.
  - So a sandbox can move data out through DNS names or to the repo host, and the audit holds at most a connect record.

  **Fix:** the owner chooses (D146 below).

  **Acceptance:** per D146. Either way, `ADR-0007:649-651` states what is actually true.

- [ ] **I285: the eBPF audit did not record the runner's connections to the model proxy**

  **Where:** the activity audit's eBPF source. ADR-0007 Q4 (`:92-96`) and `CONTEXT.md:129` say it records "network connects — allowed and blocked".

  **Problem:**
  - In run A2 the sandbox pod `r4-fence-sandbox` has 2 network records in the operator log for the whole Loop: the DNS and Gitea connects at 21:13:25 above.
  - The Loop went Planning → Implementing → Verifying. Each of the first two phases ends with the runner's answer from the model, and every model call goes through the model proxy. So the runner made at least two connections to the proxy, and none is recorded (`grep -c 'port=8080'` = 0).
  - The cause is not known. I have not separated these:
    - KubeArmor's default posture audits only some connects;
    - the relay drops or de-duplicates records;
    - the operator log is not the whole relayed stream.
  - Whatever the cause, the audit as captured does not show every connect, which is what Q4 says it does.

  **Fix:** find which of the three it is, with a run that counts the connects at each stage: KubeArmor's own output, the relay's input, and the relay's output. Then either make the eBPF source record every connect, or change Q4 and `CONTEXT.md` to say what it records.

  **Acceptance:** one kind Loop in which every connection from the sandbox to the model proxy appears in the audit, counted against the model proxy's own request count. Or a doc change that names what is not recorded, with a grep showing the old wording is gone.

## P3

- [ ] **I286: the model proxy's per-request audit has not been seen on kind**

  **Where:** `internal/proxy/proxy.go:255-277` (one JSON line per request, on the proxy's stdout); `.samples/r5/r5-lab.sh`.

  **Problem:**
  - Q4 names the model proxy as an audit source. Its lines go only to the proxy pod's stdout. That matches Q4: the cluster captures them, and Coxswain does not store them.
  - None of my lab runs saved a proxy pod's log, so no run shows these lines exist, or how many there are, on a real Loop.
  - The relay does not follow the model proxy. It follows the egress and tool proxies (`internal/alerts/discover.go:153-156`), and that is not required by Q4.

  **Fix:** the lab script saves each proxy pod's log before cleanup.

  **Acceptance:** one kind Loop. Its model proxy log has one audit line per request, and the count matches the proxy's usage reading. The same count is used for I285.

## Still open

Checked on `76b5d4d` just now:
- **I277–I280:** unchanged by #525, which touches only `internal/cni/` and `docs/OPERATOR.md`.
- **I282:** `git reflog --date=iso --since='2026-11-22 21:49' | grep -v 'pull -q --ff-only'` prints nothing: the shared tree stayed on `main` this window. I leave it open for one more round, because this window is one day.
- **I283:** #522's two P3s, not yet picked up.

The next free IDs are I287 and D150. D146 is used below, and D147–D149 by `docs/PLAN-AGENT-ADAPTERS.md` (#527). If I277 renumbers Phase 9's D140–D143, it uses D150–D153.

## Owner decision

- **D146: how should the two routes that bypass the egress proxy (I284) be handled?**
  - **(a) Recommended: close the repo route, and record the DNS names.**
    - Scope the repo-host rule to the clone. Clone in a separate step that has its own NetworkPolicy (an init Job, or the deliver Job's path), so the sandbox pod's policy has no repo rule.
    - Have the operator record the sandbox's DNS queries (names and answers), from the CNI's DNS log or a per-Loop DNS forwarder.
    - This keeps ADR-0007's claim true for everything except DNS, and makes DNS auditable.
  - **(b) Document the routes as accepted.** Change `ADR-0007:649-651` to name DNS and the repo host as audited only at the connect level. No code change. That is cheaper, but DNS stays an unrecorded way to move data out.
  - **(c) Route both through proxies.** Send DNS through a per-Loop DNS proxy, and the clone through the egress proxy. That is the strongest option, but it is the most work, and it changes how every Loop starts.

## What I did not verify

- **I285's cause.** See the item.
- **The Jobs' egress.** The verify, Judge, deliver and memory-writer Jobs have their own NetworkPolicies. I have not checked what of their traffic the audit records. The deliver Job's git push is the one that matters most.
- **The model proxy's audit lines** (I286).
