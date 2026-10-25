# Docs index

## Start here

- **[QUICKSTART.md](QUICKSTART.md)** — Get a Loop running end-to-end on a cluster: install agent-sandbox and KubeArmor, install the operator from a release's `install.yaml`, create the GitHub token Secret, run a first Loop, and read the result with `kubectl cox`.
- **[FAQ.md](FAQ.md)** — Plain-language answers to the questions a new coxswain operator is most likely to ask, taken from this `docs/` set. Each answer ends with a `See:` pointer to the section that backs it.
- **[GLOSSARY.md](GLOSSARY.md)** — Plain-language definitions of the terms an operator meets in `docs/`. Alphabetical; each entry links to where the docs define the term.

## Running it

- **[OPERATOR.md](OPERATOR.md)** — The day-to-day reference for running the coxswain operator: installing it, reading its metrics, reading its logs, and debugging a stuck Loop.
- **[TROUBLESHOOTING.md](TROUBLESHOOTING.md)** — Symptoms alpha users have hit, with the documented cause and the documented fix. Every entry cites the review doc, PR, or evidence file it comes from.
- **[UPGRADE.md](UPGRADE.md)** — Upgrade notes: what changes to watch for when moving between coxswain versions, including one-off migration steps for NetworkPolicies and other in-place objects.

## Testing

- **[EVIL-AGENT-E2E.md](EVIL-AGENT-E2E.md)** — Running the evil-agent e2e: what its probes prove, what the harness needs (and what is not in the repo yet), the verdict words, where the evidence goes, and the known gaps from the post-merge review.

## Design (ADR)

- **[0001 — Keep the name "Coxswain", use GitHub-hosted module path](adr/0001-name-collision.md)**
- **[0002 — Delegate memory curation to an LLM, rule-gated](adr/0002-llm-curation-rule-gated.md)**
- **[0003 — One-shot Loop; new work is a fork, not a re-task](adr/0003-one-shot-loop-fork-not-retask.md)**
- **[0004 — Runner reports progress via the result file only; no Loop-status RBAC](adr/0004-runner-result-file-only.md)**
- **[0005 — The operator verifies via a pinned base commit + isolated Job, not from `result.json`](adr/0005-operator-verifies-via-isolated-job.md)**
- **[0006 — Coxswain isolates *any* agent; the agent holds no credentials](adr/0006-agent-isolation-and-zero-credentials.md)**
- **[0007 — AgentPolicy — default-deny, eBPF-enforced, streamed audit](adr/0007-agentpolicy-default-deny-ebpf-audit.md)**
- **[0007 Amendment A1 — static enforcement evidence (D52)](adr/0007-amendment-a1-static-enforcement.md)**
- **[0008 — Tool proxies — credentialed per-tool, per-Loop credential boundary](adr/0008-tool-proxies.md)**
- **[0009 — Metering in the model proxy — report-only, operator-read](adr/0009-metering-in-model-proxy.md)**

## Plans and reviews

Build and design plans (`PLAN-*.md`, `TDD-PLAN-*.md`) describe how the project was structured and what each phase delivers. Review docs (`REVIEW-PHASE*.md`) are the reviewer's per-round findings; each round is one file, and the issue IDs inside them (I1, I2, …) carry forward across rounds.
