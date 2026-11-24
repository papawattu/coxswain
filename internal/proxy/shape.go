package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

// apiUsage is a usage object in any of the three model APIs the agents speak
// (docs/AGENT-HARNESSES.md, D148): OpenAI chat completions
// (prompt_tokens/completion_tokens), and Anthropic Messages and OpenAI
// Responses (input_tokens/output_tokens; Messages may add the cache counts,
// which are input the model processed).
type apiUsage struct {
	PromptTokens             json.Number `json:"prompt_tokens"`
	CompletionTokens         json.Number `json:"completion_tokens"`
	InputTokens              json.Number `json:"input_tokens"`
	OutputTokens             json.Number `json:"output_tokens"`
	CacheCreationInputTokens json.Number `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     json.Number `json:"cache_read_input_tokens"`
}

func num(n json.Number) int64 {
	v, _ := n.Int64()
	return v
}

// counts returns (prompt, completion) for whichever naming the object uses.
func (u *apiUsage) counts() (int64, int64) {
	if u.PromptTokens != "" || u.CompletionTokens != "" {
		return num(u.PromptTokens), num(u.CompletionTokens)
	}
	in := num(u.InputTokens) + num(u.CacheCreationInputTokens) + num(u.CacheReadInputTokens)
	return in, num(u.OutputTokens)
}

// parseUsage extracts the prompt/completion token counts from a non-streaming
// response body's top-level usage object, in any of the three APIs (chat
// completions, Messages, Responses all put it at the top level). It returns
// (0, 0, false) for a non-JSON body, a non-object, or an object with no usage
// object — the caller counts those as unmetered.
func parseUsage(body []byte) (prompt, completion int64, ok bool) {
	var top struct {
		Usage *apiUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Usage == nil {
		return 0, 0, false
	}
	p, c := top.Usage.counts()
	return p, c, true
}

// parseStreamUsage reads the usage from an SSE stream in any of the three APIs.
// It returns (0, 0, false) for a stream with no usage at all (unmetered). The
// "[DONE]" sentinel and non-JSON payloads are skipped.
//
//   - Chat completions: a top-level usage object, in the final chunk only
//     (forced on by shapeRequest's include_usage). Usage chunks are summed, as
//     before.
//   - Anthropic Messages: message_start carries message.usage (the input, and
//     a starting output count); each message_delta carries a CUMULATIVE usage.
//     The last value of each count is the total, so they are not summed (a sum
//     would count the output twice).
//   - OpenAI Responses: response.completed (or response.incomplete or
//     response.failed) carries the final response.usage.
func parseStreamUsage(raw []byte) (prompt, completion int64, ok bool) {
	var chatP, chatC int64
	var msgP, msgC, respP, respC int64
	chatSeen, msgSeen, respSeen := false, false, false
	for line := range bytes.SplitSeq(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Type    string    `json:"type"`
			Usage   *apiUsage `json:"usage"`
			Message *struct {
				Usage *apiUsage `json:"usage"`
			} `json:"message"`
			Response *struct {
				Usage *apiUsage `json:"usage"`
			} `json:"response"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		switch chunk.Type {
		case "message_start":
			if chunk.Message != nil && chunk.Message.Usage != nil {
				p, c := chunk.Message.Usage.counts()
				msgP, msgC, msgSeen = max(msgP, p), max(msgC, c), true
			}
		case "message_delta":
			if chunk.Usage != nil {
				p, c := chunk.Usage.counts()
				msgP, msgC, msgSeen = max(msgP, p), max(msgC, c), true
			}
		case "response.completed", "response.incomplete", "response.failed":
			if chunk.Response != nil && chunk.Response.Usage != nil {
				respP, respC = chunk.Response.Usage.counts()
				respSeen = true
			}
		default:
			if chunk.Usage != nil {
				p, c := chunk.Usage.counts()
				chatP, chatC, chatSeen = chatP+p, chatC+c, true
			}
		}
	}
	return chatP + msgP + respP, chatC + msgC + respC, chatSeen || msgSeen || respSeen
}

// streamRequested reports whether a raw top-level "stream" value asks for a
// stream (F44): anything other than absent, null or the literal false counts,
// so a coerced value ("true", 1) cannot dodge the include_usage rewrite or the
// SSE parser selection.
func streamRequested(raw json.RawMessage) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "", "false", "null":
		return false
	}
	return true
}

// shapeRequest rewrites the agent's request before it is dialed upstream so
// the agent cannot steer the meter. A streaming request (a top-level "stream"
// set to anything but absent/null/false — F44, see streamRequested) is forced
// to carry stream_options.include_usage=true so the upstream emits a final
// usage chunk. A non-JSON or non-object body is
// forwarded unmodified and the caller counts it as unmetered. The
// Accept-Encoding header is stripped separately by the proxy handler.
func shapeRequest(body []byte) (newBody []byte, isObject bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return body, false
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return body, false
	}
	// F44: any "stream" value other than the literal false/absent/null is
	// treated as streaming, so a coerced value ("true", 1) cannot dodge the
	// include_usage rewrite.
	streamVal := streamRequested(req["stream"])
	if streamVal {
		opts := map[string]json.RawMessage{}
		if raw, ok := req["stream_options"]; ok {
			_ = json.Unmarshal(raw, &opts)
		}
		opts["include_usage"] = json.RawMessage("true")
		encoded, err := json.Marshal(opts)
		if err == nil {
			req["stream_options"] = encoded
		}
	}
	out, err := json.Marshal(req)
	if err != nil {
		return body, true
	}
	return out, true
}

// drain reads a bounded number of bytes from r (bounded so a runaway
// response cannot exhaust memory; model responses are small). It returns the
// bytes actually read; a read error after some bytes is tolerated (the
// partial body is enough for metering) — a model response that is cut off
// mid-stream simply lacks its usage object and is counted unmetered.
func drain(r io.Reader, limit int64) []byte {
	b, _ := io.ReadAll(io.LimitReader(r, limit))
	return b
}
