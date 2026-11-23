# ADR-0009 amendment A1: the proxy meters Anthropic Messages and OpenAI Responses too

**Status:** Accepted (owner, 11-23, D148 (a); `docs/PLAN-AGENT-ADAPTERS.md` O2)
**Amends:** ADR-0009, "Decision" (the proxy reads `prompt_tokens` and `completion_tokens`).

## Context

Two of the agent adapters do not speak chat completions:
- **Claude Code** uses Anthropic Messages (`/v1/messages`).
- **Codex** uses OpenAI Responses (`/v1/responses`).

D148 (a): the model endpoint serves those APIs natively, which vLLM does, and the proxy meters them. Before this amendment, the proxy found no usage in either API's responses. In the T2 run (`docs/AGENT-HARNESSES.md`), 14 successful calls were logged with `usagePresent: false`, so the budget gates saw no tokens for those two agents.

## Decision

The proxy reads usage from all three APIs, still as reported by the model and never estimated.

| API | Non-streaming | Streaming |
|---|---|---|
| Chat completions | top-level `usage.prompt_tokens` and `completion_tokens` | the usage chunk (forced on with `include_usage`), summed as before |
| Messages | top-level `usage.input_tokens` (plus `cache_creation_input_tokens` and `cache_read_input_tokens`, which are input the model processed) and `output_tokens` | `message_start`'s `message.usage` and each `message_delta`'s `usage`. These are cumulative, so the last value is the total, not a sum |
| Responses | top-level `usage.input_tokens` and `output_tokens` | `response.completed`'s (or `.incomplete`'s or `.failed`'s) `response.usage` |

**The credential.** The agent holds no key (ADR-0006), so the proxy drops every auth header the agent sends: `Authorization`, `x-api-key` and `api-key`. The adapters' CLIs each send a dummy one. The proxy then injects its own key:
- in `x-api-key` for a Messages path (`/v1/messages` and below);
- in the configured header (default `Authorization: Bearer …`) for everything else.

## Evidence

`internal/proxy/api_usage_test.go` parses real vLLM output, captured byte for byte (`internal/proxy/testdata/vllm-*`). Each of the four mutations in the PR fails a spec.
