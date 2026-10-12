# Phase 1 review, round 27: R25/R26 follow-ups and the alpha topology

Round 27, 2026-10-12, since tag `review/phase1-r26`. It closes the remaining
R25 and R26 builder items, records the owner's two decisions on the alpha, and
adds user-facing troubleshooting docs. No new release: `v0.1.0-alpha.4`
stands.

| PR | Item | Merge |
|---|---|---|
| #133 | R25 I82: `cox redeliver` reports a failed delivery's reason | eb5d8c8 |
| #134 | R25 I84: AGENTS.md evidence rules (PASS, validation, citations, causes, Secrets) | 8f6a084 |
| #135 | R26 I89: model timeout 120s, 3 retries with backoff, `ModelUnavailable` | e5af830 |
| #136 | R26 I89: `ModelAvailable=False` condition on the Loop | 06a0157 |
| #137 | R26 I90: AGENTS.md (release defects, fallbacks, branches from main, kill by PID) | 90ebbcf |
| #138 | R25 I83: one PhaseAdvanced event per transition | f01ac82 |
| #139 | R26 I90: kill the PID you started, not `pgrep -x` | 50766d0 |
| #140 | `docs/TROUBLESHOOTING.md` for alpha users | 5500fb5 |
| #141 | D57: KubeArmor-only alpha, network gate documented | 686e03f |

**Status:**
- **R25 I83 reproduced and fixed.**
  - **Cause, shown not inferred:** the PhaseAdvanced event was emitted during
    claim processing, before Reconcile's status update. A Conflict on that
    update made the requeue re-process the claim and emit again, which matches
    the live trace (two events about 1s apart).
  - **Test:** a client that conflicts on the first status update gave 2
    events. The event is now emitted only after the update persists.
  - **Mutation:** emitting before the update fails the spec.
- **R26 I89 closed.**
  - The runner bounds a hung model call (about 8 minutes worst case with the
    defaults, against 10 minutes plus a restart loop before) and ends with
    `ModelUnavailable`.
  - The operator maps that to `ModelAvailable=False reason=ModelUnavailable`
    on the Loop (`kubectl cox describe`).
  - The chain is real end to end: the runner's `VerificationNotes` reach the
    claim's `blockedReason` (`runner/phase.go:518`).
- **AGENTS.md now carries** R25 I84 and R26 I90's rules.

**Owner decisions this round:**
- **D57: the alpha ships KubeArmor-only** (answers R26 Q2).
  - The KubeArmor policy gate (A1) is required.
  - The D38 network gate is documented as needing a CNI that enforces
    NetworkPolicy; Calico is the tested profile.
  - The released `install.yaml` stays **fail-closed**: on a non-enforcing CNI,
    Loops are held with `NetworkEnforced=False reason=CNIUnenforced`.
  - Users opt out explicitly with `--allow-unenforced-network`. QUICKSTART
    gives the patch command and states what it gives up: the CNI no longer
    polices pod → host-network egress (the same class as D50).
  - `config/calico` is documented as the D38 **test** profile. Its
    `--allow-unenforced` disables the policy gate, so it is not a supported
    configuration. The supported enforcing setup is Calico plus KubeArmor with
    neither flag.
- **R26 I87: the GitHub token is not rotated** (owner, 2026-10-12). The
  exposure was local only (`.samples/`, gitignored, never committed), and the
  copies were shredded.

---

## P3

### I91. TROUBLESHOOTING cites the wrong release's A6 run

- [ ] Open

**Where:** `docs/TROUBLESHOOTING.md`, the Service DNS section.

**Problem:** it calls `.samples/alpha-e2e-1791788025/dns-eth0.log` "the
alpha.3 A6 run". That run used `coxswain-controller:v0.1.0-alpha.2`. The
alpha.3 acceptance run is `alpha-e2e-1791806082`, and it shows the same result
(`Server: 10.96.0.10`, `Address: 10.96.0.1`).

**Fix:** cite the alpha.3 run.

**Acceptance:** the cited directory's `digests.txt` shows alpha.3.

## Questions for the owner

None open. Q2 is answered by D57.

## Carried

- R22 I59 (design) and I62 (workflow edits are an owner push): open as
  written.

## Next

1. I91, a one-line docs fix.
2. The next phase is the owner's call. `docs/PLAN-ALPHA.md` lists what the
   alpha deferred, including PLAN.md Phase 3: snapshots, retention,
   rollback and fork.
