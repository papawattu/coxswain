# Delegate memory curation to an LLM, rule-gated

The operator is deterministic by design (Q1), but the user wants self-learning across Loops. Curation *is* content inspection, which would break that boundary.

We resolved the tension by splitting it: **static rules** (Loop ended, `lessons[]` present, ≤500 chars, references a file path or error signature, no secrets, dedup by hash) are enforced by the operator in code. Only when *all* rules pass does the operator invoke a **Memory Curator** LLM to make the single judgment call "is this lesson worth keeping?" The LLM's verdict (boolean + one-line reason) is written to the audit trail; it never rewrites the lesson text.

The boundary, precisely: the operator never interprets content *itself*, but it may *ask* a model to and *record* the answer. This is the one place the "deterministic operator" rule has a controlled exception.

**Considered and rejected:**
- Pure rule-based curation (no LLM): deterministic but can't judge relevance/quality; low-value or misleading lessons would pollute memory.
- Fully LLM-curated (operator inspects content): violates the determinism/auditability requirement that drove the whole architecture.
