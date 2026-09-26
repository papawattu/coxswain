# Phase 0 review, round 10 — open issues

Review of `6f67407`..`9555cdf` (2026-09-26), since tag `review/phase0-r9`.
Baseline at review time: `internal/controller` envtest green, root
golangci-lint 0 issues, runner green.

Each issue is self-contained so it can be picked up independently. Work
test-first as in `docs/TDD-PLAN.md`: write the failing test at the named seam,
then fix. Tick the box and add the commit hash when done. Issue IDs continue
from earlier rounds so every ID is unique.

Priority: **P1** = fix before Phase 1 starts. **P2** = decide during Phase 1
(design). **P3** = cleanup, batch whenever.

## Verdicts on reviewed commits

| Commit | Issue | Verdict | Follow-ups |
|--------|-------|---------|------------|
| `6f67407` | I15 plan slices B3a/B3b/B3c | OK + notes | I16 |
| `484064a` | bookkeeping | OK | — |
| `794045b` | R1 P3 license headers | OK | — |
| `924cf48` | R1 P3 `maxIterations` default | OK + notes | I19 |
| `ef0d6bb` | R1 P3 `Ref` marker / SSH remotes | OK + notes | I18 |
| `7633467` | R1 P3 `go mod tidy` | OK | — |
| `9555cdf` | R1 P3 controller tidy-ups | OK + notes | **D20** |

The round-1 P3 batch is done except the e2e suite (deliberately deferred: it
needs a kind cluster, which is the owner's call). Tick those items in
`REVIEW-PHASE0.md`.

---

## P2 — Design

### D20. Sandbox names must be DNS-1035 labels — agent-sandbox names a Service after them

- [ ] Decided

**Where:** `internal/controller/loop_controller.go` `sandboxName`;
`api/v1alpha1/loop_types.go` (Loop CRD validation).

**Problem:** agent-sandbox v1.0.4 creates a `corev1.Service` named
`sandbox.Name` (`controllers/sandbox_controller.go:1059-1070`). Service
names must be **DNS-1035 labels**: lowercase alphanumerics and `-`, **start
with a letter**, ≤63 chars, **no dots**. Loop names are DNS-1123
*subdomains*: may contain `.`, may start with a digit, up to 253 chars. So:
- `fix.auth` → Sandbox `fix.auth-sandbox` is accepted, but agent-sandbox's
  Service create fails; the sandbox never becomes ready.
- `1-bug` → `1-bug-sandbox` fails the same way (starts with a digit).
`9555cdf`'s hash-truncation only handles length.

**Fix (recommended):** reject such Loop names at admission with CRD CEL
validation on the root object, so users get a clear error at `kubectl
apply` instead of a stuck sandbox:

```go
// +kubebuilder:validation:XValidation:rule="self.metadata.name.matches('^[a-z]([-a-z0-9]*[a-z0-9])?$') && size(self.metadata.name) <= 55",message="Loop name must be a DNS-1035 label of at most 55 characters (it names the Sandbox and its Service)"
```

(55 = 63 − len(`-sandbox`)). Then `sandboxName` is just `name + "-sandbox"`
and the hash-truncation + UTF-8 back-off can go — Kubernetes names are ASCII,
so the rune loop was dead code anyway. Alternative if you'd rather accept any
name: sanitize (`.`→`-`, prefix a letter when it starts with a digit) *and*
hash-suffix — but then two Loops can map to similar names and adoption-by-name
(S2) gets murkier. Prefer validation.

**Acceptance:** envtest: creating Loops named `fix.auth`, `1-bug`, and a
56-char name fails with the validation message; a 55-char DNS-1035 name
succeeds and its Sandbox is `<name>-sandbox`; `loop_sandboxname_test.go`
updated/removed accordingly.

---

## P3 — Cleanup

### I16. D17 mitigation 3 (advisory diff scan) has no plan slice

- [ ] Done

ADR-0005 D17 lists three mitigations; `TDD-PLAN-PHASE1.md` has B3a
(canary) but nothing for the advisory static scan (grep the base→verified
diff of non-protected files for `testing.Testing()`, `os.Exit` in `init`,
`//go:linkname`, `flag.Lookup("test.`, emit a `Warning` event + history note,
force `spec.pr.ready=false`). Add it as **B3d**, advisory only (never a gate),
or mark it explicitly deferred to Phase 6 in both the ADR and the plan.

### I18. `Workspace.Repo` lost all validation; review notes leaked into the API docs

- [ ] Done

**Where:** `api/v1alpha1/loop_types.go` (`ef0d6bb`).

- Dropping `Format=uri` left `repo` accepting any string, including `""`.
  Add `+kubebuilder:validation:MinLength=1` and a pattern accepting
  `https://…`, `ssh://…`, and scp-style `user@host:path`, e.g.
  `^(https://|ssh://|[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:).+`. envtest: `""` and
  `not a url` rejected; `https://github.com/x/y.git` and
  `git@github.com:x/y.git` accepted.
- The field comments now say "the Format=uri constraint was dropped (P3)" and
  "the +kubebuilder:default="" marker was a no-op and removed — P3". Those
  comments become the CRD descriptions users read in `kubectl explain`. Keep
  field docs about the field; history belongs in commit messages. Regenerate
  the CRD after trimming.

### I19. `loop_defaults_test.go` cleanup uses a cancelled context

- [ ] Done

`AfterEach` calls `cancel()` and then `k8sClient.Delete(ctx, ns)` with the
same, now-cancelled `ctx`, so the delete always fails silently. Delete first,
then cancel (or use `context.Background()` for cleanup). Also, CRD defaulting
is applied synchronously at create — the `Eventually` can be a plain `Get` +
`Expect`.

---

## Status and next steps

With this round, everything the builder can do without the owner is either
done or listed above. Remaining after D20/I16/I18/I19:

- **I4 (P1) — owner decision.** Blocks Phase 0 sign-off.
- **D5, D6 (P2) — owner decisions** (k8s version pin; snapshots on kind).
- **Round-1 e2e suite (P3)** — needs a kind cluster; owner's call.

**Builder: after D20, I16, I18, I19, stop and wait for the owner.** Don't
start Phase 1 slice B1 until I4 is decided — Phase 1's scope depends on it
(option (a) moves the runner-in-sandbox work into Phase 1).
