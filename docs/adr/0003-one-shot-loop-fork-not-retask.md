# One-shot Loop; new work is a fork, not a re-task

A `Loop` carries exactly one goal and ends in `Succeeded` or `Failed`. The user considered letting a running Loop be re-tasked with a new goal mid-life and rejected it.

**Why one-shot:** the reconcile state machine stays a total function on finite input. Re-scoping a live Loop forces open what happens to the running history, the accumulated budget, the open branch, and the approved plan hash — each of which is pinned to the *original* goal. A fork-from-checkpoint (Phase 3) gives the same continuity (same workspace, same accumulated memory) without any of that state-machine complexity.

**Consequence:** "long-lived" means *many iterations within one Loop*, not a Loop that lives across many goals. If a human wants to change direction, they `cox fork <loop> --from iter-N` and point the new Loop at the new goal.
