# coxswain - AI Agent Guide

## Review protocol

A separate reviewer agent reviews the builder's work. **Code is reviewed on
pull requests; design is reviewed in review docs.**

### Pull requests (code — from Phase 1 slice C2 onward)

- **Never commit to `main` directly.** One branch and one PR per slice or
  review item: `slice/<id>-<short-name>` (e.g. `slice/c2-model-proxy`) or
  `fix/<id>-<short-name>` (e.g. `fix/i34-agent-env`).
- **Open the PR as a draft early** and push as you go. The PR description
  names the slice / issue IDs it closes and how it was verified (tests, and
  a kind run for anything that changes the sandbox pod).
- **Mark it ready for review** when CI (`make test`, `make lint`, and the
  kind e2e once it exists) is green and the slice's acceptance is met.
  (D40: the builder marks PRs ready via the GraphQL API —
  `gh api graphql -f query="mutation{markPullRequestReadyForReview(input:{pullRequestId:\"$ID\"}){pullRequest{isDraft}}}"`
  where `ID=$(gh api repos/<owner>/<repo>/pulls/<n> --jq .node_id)`. The
  `gh pr ready` CLI command requires the `workflow` scope which the
  harness-injected token lacks; the GraphQL mutation works with the
  stored fine-grained PAT.)
- **The reviewer reviews on the PR with comment reviews, not approvals.** The
  builder, reviewer and owner all act through the same GitHub account, and GitHub
  does not let an account approve its own PR — so a required approval could never
  be satisfied. Instead: the reviewer submits a **Comment** review whose body
  starts with the verdict (`Verdict: OK`, `Verdict: OK + notes`, or
  `Verdict: CHANGES`). Each finding is an inline comment. P1 findings stay
  **unresolved** until fixed; **required conversation resolution** makes them
  block the merge. The builder addresses each comment with a follow-up commit on
  the branch (don't force-push over reviewed commits) and replies on the thread.
- **The builder never resolves a review thread.** It pushes the fix and
  replies with what changed and how it was verified; the **reviewer**
  re-checks and resolves the thread (or replies with what's still wrong). A
  later review with `Verdict: OK` supersedes an earlier `Verdict: CHANGES`.
- **The owner merges** when the verdict is OK and no threads are open. Neither
  the builder nor the reviewer merges to `main`. Squash-merge with the PR title
  as the subject (`C2: …`, `I34: …`).
- **After merge:** delete the branch, pull `main`, and start the next slice
  from it.

### Review docs (design, cross-cutting issues, owner decisions)

- **Review docs:** `docs/REVIEW-PHASE<N>.md` (round 1) and
  `docs/REVIEW-PHASE<N>-R<k>.md` (round k). All rounds share one format:
  P1/P2/P3 issues, each with a checkbox, **Where / Problem / Fix /
  Acceptance**. Issue IDs (`I<n>`, `D<n>`) are unique across rounds.
- **Each review is one commit** with subject `review(phase<N>-r<k>): …`,
  touching `docs/` only, tagged `review/phase<N>-r<k>` (lightweight tag).
  Review docs go through a PR too (branch `review-phase<N>-r<k>`, hyphens so it never clashes with the tag) once `main`
  is protected; the tag is applied to the merged commit.
- **What goes where:** a finding about specific lines of code → PR review
  comment. A finding about design, an ADR, the plan, or something spanning
  several slices, or a question for the owner → review doc.
- **Builder: watch for new reviews** with `git tag -l 'review/*'` (or
  `git log --grep '^review('`). Before each work session, read any review
  tagged since your last one. P1 items block the next phase.
- **Closing an issue:** tick its box in the review doc and add the fixing
  commit hash. Reference the issue ID in the fix commit subject
  (e.g. `I6: …`). Don't edit other parts of a review doc; reply to a
  verdict by adding a `Builder response:` line under the issue.
  **Tick only after the fix has MERGED, and cite the squash hash** — the
  merge commit's subject on `main`, not the branch HEAD or a pre-merge hash
  (I76: a box ticked from a branch hash before merge is wrong and unmergeable
  in place). The tick is part of a review-doc PR (a later round or a
  housekeeping commit), never on the fix branch itself while it is still
  unmerged.
- **After replying to review threads, confirm no review is stuck PENDING**
  (I76). A reply posted while a review is in the PENDING state is invisible
  and blocks the reviewer's review. After posting replies, run
  `gh api repos/<owner>/<repo>/pulls/<n>/reviews --jq '[.[]|select(.state=="PENDING")|.id]'
  and confirm it prints `[]` before ending the turn. If a review is PENDING,
  submit it yourself (event COMMENT — builder, reviewer and owner share one
  GitHub account, so there is no "not yours") and re-run the check until it
  prints `[]`.
- **Work a queue of items to completion without stopping to ask** (I76). When
  handed a list of independent review items, carry on through the whole queue
  — branch, fix, PR, reply, mark ready — and only stop at the end of the
  queue or at a genuine blocker (a decision that needs the owner, a host
  memory limit, a credential scope). Don't pause after each item to ask
  "shall I continue?"; the queue is the instruction.
- **Stage explicit paths, never `git add -A`** (I31). The owner and reviewer
  both edit files in the same tree; `git add -A` sweeps their in-progress edits
  (e.g. the owner's `CONTEXT.md` direction change) into builder commits and
  mis-attributes product-direction changes to bug fixes. Always `git add
  <explicit files>` so a commit contains only what the builder changed. Don't
  rewrite history to fix an accidental sweep — note it and use explicit paths
  from then on.
- **Never commit evidence or knowledge dirs** (I68). `.samples/` and `.gnosis/`
  are never committed. Never `git add -f` under them. If a file under either
  path is tracked, `make lint` fails (the guard below).
- **Quote plan/ADR text only after grepping it** (I68). Before citing a line
  from a plan, ADR, or review doc, grep it to confirm it exists, and cite the
  file and line (e.g. `docs/TDD-PLAN-PHASE2.md:1245`). Don't justify an
  out-of-scope change with a plan line that doesn't exist.
- **Use only your own credentials; never search for, read or print other
  credentials** (I58, D48). That covers tokens, PATs, env and secret files,
  and credential stores. Never run `gh auth token` (it prints the session's
  GitHub token in full). If a push is rejected (e.g. a workflow scope
  limitation), report it and stop — do not go looking for other credentials
  to retry. `.github/workflows` edits are an owner push: the builder stops
  and reports, and the owner pushes that commit.

## Project Structure

**Single-group layout (default):**
```
cmd/main.go                    Manager entry (registers controllers/webhooks)
api/<version>/*_types.go       CRD schemas (+kubebuilder markers)
api/<version>/zz_generated.*   Auto-generated (DO NOT EDIT)
internal/controller/*          Reconciliation logic
internal/webhook/*             Validation/defaulting (if present)
config/crd/bases/*             Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/*               Example CRs (edit these)
Makefile                       Build/test/deploy commands
PROJECT                        Kubebuilder metadata Auto-generated (DO NOT EDIT)
```

**Multi-group layout** (for projects with multiple API groups):
```
api/<group>/<version>/*_types.go       CRD schemas by group
internal/controller/<group>/*          Controllers by group
internal/webhook/<group>/<version>/*   Webhooks by group and version (if present)
```

Multi-group layout organizes APIs by group name (e.g., `batch`, `apps`). Check the `PROJECT` file for `multigroup: true`.

**To convert to multi-group layout:**
1. Run: `kubebuilder edit --multigroup=true`
2. Move APIs: `mkdir -p api/<group> && mv api/<version> api/<group>/`
3. Move controllers: `mkdir -p internal/controller/<group> && mv internal/controller/*.go internal/controller/<group>/`
4. Move webhooks (if present): `mkdir -p internal/webhook/<group> && mv internal/webhook/<version> internal/webhook/<group>/`
5. Update import paths in all files
6. Fix `path` in `PROJECT` file for each resource
7. Update test suite CRD paths (add one more `..` to relative paths)

## Critical Rules

### Never Edit These (Auto-Generated)
- `config/crd/bases/*.yaml` - from `make manifests`
- `config/rbac/role.yaml` - from `make manifests`
- `config/webhook/manifests.yaml` - from `make manifests`
- `**/zz_generated.*.go` - from `make generate`
- `PROJECT` - from `kubebuilder [OPTIONS]`

### Never Remove Scaffold Markers
Do NOT delete `// +kubebuilder:scaffold:*` comments. CLI injects code at these markers.

### Keep Project Structure
Do not move files around. The CLI expects files in specific locations.

### Always Use CLI Commands
Always use `kubebuilder create api` and `kubebuilder create webhook` to scaffold. Do NOT create files manually.

### E2E Tests Require an Isolated Kind Cluster
The e2e tests are designed to validate the solution in an isolated environment (similar to GitHub Actions CI).
Ensure you run them against a dedicated [Kind](https://kind.sigs.k8s.io/) cluster (not your “real” dev/prod cluster).

## After Making Changes

**After editing `*_types.go` or markers:**
```
make manifests  # Regenerate CRDs/RBAC from markers
make generate   # Regenerate DeepCopy methods
```

**After editing `*.go` files:**
```
make lint-fix   # Auto-fix code style
make test       # Run unit tests
```

### Test norms (R16 I43; R20 I49; R21 I55)

- Every object the controller reconciles gets at least one **same-Loop**
  envtest spec that changes an input and re-reconciles, asserting the object
  is **updated** (or deleted) — not just created. (The I42c finding: a
  no-op mutate froze NetworkPolicy specs at creation and no spec caught it.)
- Every gate spec must **FAIL when the gate is disabled**: disable the gate in
  a scratch copy, run the spec, and confirm it fails before claiming a gate
  is tested. Don't commit the mutation; record the result in the PR.
- Every decision that reads pod, container, or Job status gets a spec for
  **each in-progress state** (Waiting, Running, a Job with neither Failed nor
  Succeeded) as well as each terminal state, asserting **no decision** while
  in progress (the I49/S5a finding: `verifyOutcome` mapped a still-running
  check to "iterate").
- Mutations run in a **scratch worktree** (`git worktree add`), never in the
  working tree. Record the result; delete the worktree.
- **After any mutation KIND run, redeploy a build of `main` and say so in the
  PR** (I76). A mutation operator left deployed (e.g. a budget-gate-disabled
  build) is a live hazard: it masks the real behaviour and can confuse a
  later reviewer or run. Redeploy the real `main` image after the mutation
  run, and record in the PR that the cluster was restored to `main`.
- Kind-run logs and generated evidence are **never discarded or redirected to
  `/dev/null`**.
- **Paste command output for any "shown working" claim; never state an
  unverified root cause** (I76). A claim that something works on kind (or
  anywhere) must carry the actual command + output in the PR or review reply,
  not a bare assertion. A doc comment or message that asserts *why* something
  happened (a root cause) is only allowed when that cause was actually
  verified (a kind test, a log, a repro); otherwise state that the cause is
  unknown and the observation is unexplained. Don't write a confident cause
  that a test disproves.
- Any shell script embedded in Go (Job or init-container scripts) has an
  **execution test**: it runs the real generated script with only path or host
  constants substituted, before any kind run. A standalone hack script (e.g.
  `hack/sample-run.sh`) is verified the same way: **execute the real changed
  block with stubbed commands**, not a re-implementation. Copy the exact lines
  into a harness that stubs the external commands (a fake `kubectl` on the
  PATH), and run it — don't paraphrase the logic into a test that may diverge
  from the committed script.
- When a reviewer names a mutation, the builder applies **exactly** that diff
  in a scratch worktree and records the result. A broader mutation doesn't
  count.

## CLI Commands Cheat Sheet

### Create API (your own types)
```bash
kubebuilder create api --group <group> --version <version> --kind <Kind>
```

### Deploy Image Plugin (scaffold to deploy/manage ANY container image)

Generate a controller that deploys and manages a container image (nginx, redis, memcached, your app, etc.):

```bash
# Example: deploying memcached
kubebuilder create api --group example.com --version v1alpha1 --kind Memcached \
  --image=memcached:alpine \
  --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Scaffolds good-practice code: reconciliation logic, status conditions, finalizers, RBAC. Use as a reference implementation.


### Create Webhooks
```bash
# Validation + defaulting
kubebuilder create webhook --group <group> --version <version> --kind <Kind> \
  --defaulting --programmatic-validation

# Conversion webhook (for multi-version APIs)
kubebuilder create webhook --group <group> --version v1 --kind <Kind> \
  --conversion --spoke v2
```

### Controller for Core Kubernetes Types
```bash
# Watch Pods
kubebuilder create api --group core --version v1 --kind Pod \
  --controller=true --resource=false

# Watch Deployments
kubebuilder create api --group apps --version v1 --kind Deployment \
  --controller=true --resource=false
```

### Controller for External Types (e.g., from other operators)

Watch resources from external APIs (cert-manager, Argo CD, Istio, etc.):

```bash
# Example: watching cert-manager Certificate resources
kubebuilder create api \
  --group cert-manager --version v1 --kind Certificate \
  --controller=true --resource=false \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

**Note:** Use `--external-api-module=<module>@<version>` only if you need a specific version. Otherwise, omit `@<version>` to use what's in go.mod.

### Webhook for External Types

```bash
# Example: validating external resources
kubebuilder create webhook \
  --group cert-manager --version v1 --kind Issuer \
  --defaulting \
  --external-api-path=github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1 \
  --external-api-domain=io \
  --external-api-module=github.com/cert-manager/cert-manager
```

## Testing & Development

```bash
make test              # Run unit tests (uses envtest: real K8s API + etcd)
make run               # Run locally (uses current kubeconfig context)
```

Tests use **Ginkgo + Gomega** (BDD style). Check `suite_test.go` for setup.

## Deployment Workflow

```bash
# 1. Regenerate manifests
make manifests generate

# 2. Build & deploy
export IMG=<registry>/<project>:tag
make docker-build docker-push IMG=$IMG  # Or: kind load docker-image $IMG --name <cluster>
make deploy IMG=$IMG

# 3. Test
kubectl apply -k config/samples/

# 4. Debug
kubectl logs -n <project>-system deployment/<project>-controller-manager -c manager -f
```

### API Design

**Key markers for** `api/<version>/*_types.go`:

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Status",type=string,JSONPath=".status.conditions[?(@.type=='Ready')].status"

// On fields:
// +kubebuilder:validation:Required
// +kubebuilder:validation:Minimum=1
// +kubebuilder:validation:MaxLength=100
// +kubebuilder:validation:Pattern="^[a-z]+$"
// +kubebuilder:default="value"
```

- **Use** `metav1.Condition` for status (not custom string fields)
- **Use predefined types**: `metav1.Time` instead of `string` for dates
- **Follow K8s API conventions**: Standard field names (`spec`, `status`, `metadata`)

### Controller Design

**RBAC markers in** `internal/controller/*_controller.go`:

```go
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=mygroup.example.com,resources=mykinds/finalizers,verbs=update
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
```

**Implementation rules:**
- **Idempotent reconciliation**: Safe to run multiple times
- **Re-fetch before updates**: `r.Get(ctx, req.NamespacedName, obj)` before `r.Update` to avoid conflicts
- **Structured logging**: `log := log.FromContext(ctx); log.Info("msg", "key", val)`
- **Owner references**: Enable automatic garbage collection (`SetControllerReference`)
- **Watch secondary resources**: Use `.Owns()` or `.Watches()`, not just `RequeueAfter`
- **Finalizers**: Clean up external resources (buckets, VMs, DNS entries)

### Logging

**Follow Kubernetes logging message style guidelines:**

- Start from a capital letter
- Do not end the message with a period
- Active voice: subject present (`"Deployment could not create Pod"`) or omitted (`"Could not create Pod"`)
- Past tense: `"Could not delete Pod"` not `"Cannot delete Pod"`
- Specify object type: `"Deleted Pod"` not `"Deleted"`
- Balanced key-value pairs

```go
log.Info("Starting reconciliation")
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

**Reference:** https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines

### Webhooks
- **Create all types together**: `--defaulting --programmatic-validation --conversion`
- **When`--force`is used**: Backup custom logic first, then restore after scaffolding
- **For multi-version APIs**: Use hub-and-spoke pattern (`--conversion --spoke v2`)
  - Hub version: Usually oldest stable version (v1)
  - Spoke versions: Newer versions that convert to/from hub (v2, v3)
  - Example: `--group crew --version v1 --kind Captain --conversion --spoke v2` (v1 is hub, v2 is spoke)

### Learning from Examples

The **deploy-image plugin** scaffolds a complete controller following good practices. Use it as a reference implementation:

```bash
kubebuilder create api --group example --version v1alpha1 --kind MyApp \
  --image=<your-image> --plugins=deploy-image.go.kubebuilder.io/v1-alpha
```

Generated code includes: status conditions (`metav1.Condition`), finalizers, owner references, events, idempotent reconciliation.

## Distribution Options

### Option 1: YAML Bundle (Kustomize)

```bash
# Generate dist/install.yaml from Kustomize manifests
make build-installer IMG=<registry>/<project>:tag
```

**Key points:**
- The `dist/install.yaml` is generated from Kustomize manifests (CRDs, RBAC, Deployment)
- Commit this file to your repository for easy distribution
- Users only need `kubectl` to install (no additional tools required)

**Example:** Users install with a single command:
```bash
kubectl apply -f https://raw.githubusercontent.com/<org>/<repo>/<tag>/dist/install.yaml
```

### Option 2: Helm Chart

```bash
kubebuilder edit --plugins=helm/v2-alpha                      # Generates dist/chart/ (default)
kubebuilder edit --plugins=helm/v2-alpha --output-dir=charts  # Generates charts/chart/
```

**For development:**
```bash
make helm-deploy IMG=<registry>/<project>:<tag>          # Deploy manager via Helm
make helm-deploy IMG=$IMG HELM_EXTRA_ARGS="--set ..."    # Deploy with custom values
make helm-status                                         # Show release status
make helm-uninstall                                      # Remove release
make helm-history                                        # View release history
make helm-rollback                                       # Rollback to previous version
```

**For end users/production:**
```bash
helm install my-release ./<output-dir>/chart/ --namespace <ns> --create-namespace
```

**Important:** If you add webhooks or modify manifests after initial chart generation:
1. Backup any customizations in `<output-dir>/chart/values.yaml` and `<output-dir>/chart/manager/manager.yaml`
2. Re-run: `kubebuilder edit --plugins=helm/v2-alpha --force` (use same `--output-dir` if customized)
3. Manually restore your custom values from the backup

### Publish Container Image

```bash
export IMG=<registry>/<project>:<version>
make docker-build docker-push IMG=$IMG
```

## References

### Essential Reading
- **Kubebuilder Book**: https://book.kubebuilder.io (comprehensive guide)
- **controller-runtime FAQ**: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md (common patterns and questions)
- **Good Practices**: https://book.kubebuilder.io/reference/good-practices.html (why reconciliation is idempotent, status conditions, etc.)
- **Logging Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines (message style, verbosity levels)

### API Design & Implementation
- **API Conventions**: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
- **Operator Pattern**: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
- **Markers Reference**: https://book.kubebuilder.io/reference/markers.html

### Tools & Libraries
- **controller-runtime**: https://github.com/kubernetes-sigs/controller-runtime
- **controller-tools**: https://github.com/kubernetes-sigs/controller-tools
- **Kubebuilder Repo**: https://github.com/kubernetes-sigs/kubebuilder
