package proxy

import (
	"bytes"
	"encoding/json"
	"io"
)

// parseUsage extracts the prompt/completion token counts from a non-streaming
// response body's top-level usage object. It returns (0, 0, false) for a
// non-JSON body, a non-object, or an object with no usage object — the
// caller counts those as unmetered.
func parseUsage(body []byte) (prompt, completion int64, ok bool) {
	var top struct {
		Usage *struct {
			PromptTokens     json.Number `json:"prompt_tokens"`
			CompletionTokens json.Number `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &top); err != nil || top.Usage == nil {
		return 0, 0, false
	}
	p, _ := top.Usage.PromptTokens.Int64()
	c, _ := top.Usage.CompletionTokens.Int64()
	return p, c, true
}

// parseStreamUsage accumulates the usage object across an SSE stream. It scans
// every "data: ..." line, decodes the JSON payload, and returns the sum of any
// usage objects plus whether at least one usage chunk was present. A stream
// that ends without a usage chunk returns (0, 0, false) — unmetered. The
// standard OpenAI stream carries the final usage in its last data chunk
// (forced on by shapeRequest's include_usage); the "[DONE]" sentinel is not
// JSON and is skipped.
func parseStreamUsage(raw []byte) (prompt, completion int64, ok bool) {
	seen := false
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
			Usage *struct {
				PromptTokens     json.Number `json:"prompt_tokens"`
				CompletionTokens json.Number `json:"completion_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue
		}
		if chunk.Usage == nil {
			continue
		}
		seen = true
		p, _ := chunk.Usage.PromptTokens.Int64()
		c, _ := chunk.Usage.CompletionTokens.Int64()
		prompt += p
		completion += c
	}
	return prompt, completion, seen
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
