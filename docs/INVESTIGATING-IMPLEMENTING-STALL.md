# A3 e2e stall in Implementing — investigation (2026-10-12)

Investigation PR (draft, no code change): the three consecutive a3 e2e
failures (2026-10-12, `ph=Implementing` after 25 min) are **environmental**
— the vLLM backends behind the nginx LB stopped serving the runner's model
calls while the LB stayed healthy. Live kind runs on a build of main
(213dacf) show the identical path completing in ~4 min.

Full findings + pasted evidence: the investigation PR's description.
Raw live-run evidence (not committed, I68): `.samples/
implementing-stall-1791775839/` and `.samples/implementing-stall-
1791776444/` (phase.log, proxy-stream.log, operator-stream.log,
implementing-watch.log, events-stream.log) and the three failed runs'
`.samples/a3-cli-e2e-179177*/`.

## Open items (need the vLLM host — not reachable from this host)

1. LB config on 192.168.1.20: per-backend health checks / weights; did
   backend .36 (834 completed requests vs .37's 22932) stay weighted while
   down during the failure window (02:05-02:49)?
2. vLLM logs on 192.168.1.36/.37 for the failure window.
3. Owner decision: raise the e2e's phase wait (300 -> >= 600 x 4 s) or add
   a vLLM health preflight, so a hung endpoint is distinguishable from a
   regression at the 25-minute mark (docs/timeout change; does not change
   what the e2e proves).

## Kind state

Restored to main (operator `coxswain-controller:main-ab538d1-20261012031040`,
digest-identical to a fresh 213dacf build; `--runner-image=
coxswain-runner:dev`). Both investigation Loops deleted; no kind cluster
touched or deleted.
