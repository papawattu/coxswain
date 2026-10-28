// Copyright 2026 papawattu.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package alerts consumes the enforcement engines' audit streams (F62/F63,
// ADR-0007 Q4/Q5).
//
// The operator does not trust the agent for the count: the alerts are read from
// the engine's OWN container log (and from the operator's own proxy pods),
// followed over pods/log — never polled. A poller cannot tell a rotation gap
// from "nothing happened", which is exactly the I32 defect this replaces.
//
// Two consumers live on one stream:
//
//   - F62: a per-Loop count of BLOCKED actions, which the Loop controller reads
//     when it reconciles and writes to status.policy.blockedCount plus the
//     PolicyBlocked condition. The count is keyed by Loop UID (a sandbox pod is
//     recreated per phase; the ADR counts per Loop).
//   - F63: one JSON line per alert (allowed and blocked) on the operator's
//     stdout in the Q4 envelope, with `iteration` joined from the Loop status
//     at emit time — the proxies cannot fill it (their env is immutable), which
//     is R31 I107 row 24.
package alerts

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"
)

// Reasons the PolicyBlocked condition carries when a count cannot be trusted.
// They are stable strings (the condition reason is user-facing vocabulary).
const (
	// ReasonStreamUnavailable is a stream that never attached (a pods/log read
	// that failed, no permission, no pod) — the count is UNKNOWN, not 0 (I32).
	ReasonStreamUnavailable = "AuditStreamUnavailable"
	// ReasonStreamGap is a re-attach whose first line is later than the last
	// accepted alert: a rotation window the relay cannot rule out, so the count
	// is UNKNOWN rather than a smaller number.
	ReasonStreamGap = "AuditStreamGap"
	// ReasonEngineUnavailable is "no engine agent pod was discovered at all":
	// the count cannot exist, which is different from a stream that broke.
	ReasonEngineUnavailable = "EngineUnavailable"
	// ReasonUndecodable is a run of lines the relay could not decode: the
	// stream is not producing the audit format it expected, so it cannot be
	// trusted either. One bad line is tolerated (see maxDecodeFailures).
	ReasonUndecodable = "AuditStreamUndecodable"
)

// Action values a parsed alert can carry. The engine's block verdict is the
// one the count is about.
const (
	ActionBlock = "Block"
	ActionAllow = "Allow"
)

// Audit source names carried in the Q4 envelope's `source` field.
const (
	SourceKubeArmor = "kubearmor"
	SourceEgress    = "egress-proxy"
	SourceToolProxy = "tool-proxy"
)

// Health is the relay's assessment of the stream(s) feeding one Loop. Only
// HealthHealthy may publish a number: every other state means "unknown", never
// "zero" (I32 / ADR-0007 Q5).
type Health string

const (
	// HealthUnknown is "no news yet": no stream for this Loop has attached and
	// nothing has failed. The controller writes nothing at all.
	HealthUnknown Health = ""
	// HealthHealthy is a stream attached and readable; the count is a number.
	HealthHealthy Health = "Healthy"
	// HealthGap is a stream that re-attached and cannot prove it saw every
	// alert between the last one and the re-attach.
	HealthGap Health = "Gap"
	// HealthUnavailable is a stream that never attached, or the engine is
	// absent: there is no evidence to count.
	HealthUnavailable Health = "Unavailable"
)

// Alert is one decoded audit line, normalised across the sources (the engine's
// KubeArmor JSON and the operator's own proxy records).
type Alert struct {
	// Time is the line's own timestamp (the engine's or the proxy's), never
	// the relay's clock: the re-attach window and the replay check both key on
	// it.
	Time time.Time
	// Action is the engine's operation/action verb (Block, Allow, "connect",
	// "request").
	Action string
	// Blocked reports whether this line is a BLOCK verdict.
	Blocked bool
	// Operation is the engine's operation class (Process, Network, File, …).
	Operation string
	// Namespace and Pod name the workload the alert is about — for the engine
	// that is the SANDBOX pod (the agent pod is where the line is read from),
	// for the proxies it is the Loop's namespace and the Loop name is carried
	// in Loop.
	Namespace string
	Pod       string
	Container string
	// Loop and LoopNamespace are set by the proxy sources, which already know
	// the Loop (their records carry it). The engine lines leave them empty: the
	// relay attributes those through the sandbox pod's operator-set label.
	Loop          string
	LoopNamespace string
	// Target is what the alert is about (a file path, an endpoint, a tool
	// request) — the value a human needs to act on the block.
	Target string
	// Source is one of the Source* constants.
	Source string
	// Detail is free-form correlation data (the policy name/hash, the
	// resolved IP, the HTTP status).
	Detail string
}

// Verdict renders the alert's verdict for the Q4 envelope: the engine says
// "Block", the proxies say "blocked"; the envelope uses the same word for both.
func (a Alert) Verdict() string {
	if a.Blocked {
		return "blocked"
	}
	return "allowed"
}

// Record is the Q4 audit envelope (ADR-0007 Q4; CONTEXT.md:129): every alert,
// allowed and blocked, becomes exactly one JSON line in this shape.
type Record struct {
	Time      string `json:"time"`
	Loop      string `json:"loop"`
	Namespace string `json:"namespace"`
	Iteration string `json:"iteration"`
	Source    string `json:"source"`
	Action    string `json:"action"`
	Target    string `json:"target"`
	Verdict   string `json:"verdict"`
	Detail    string `json:"detail"`
}

// WriteRecord writes the record as one JSON line, newline-terminated (the form
// an operator's `kubectl logs` shows one-per-line).
func WriteRecord(w io.Writer, rec Record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = w.Write(line)
	return err
}

// LineKind classifies a log line so the relay can tell ordinary container
// output from a line that SHOULD be an audit record and is not (E1: counting
// startup noise as "the format changed" degraded every Loop to Unknown).
type LineKind int

const (
	// LineNoise is ordinary container output (a zap text line, an unrelated
	// JSON object): skipped silently, never a decode failure.
	LineNoise LineKind = iota
	// LineMalformed looks like an audit record of one of the three sources and
	// still failed to decode (truncated, bad timestamp): a real failure.
	LineMalformed
	// LineAlert is a decoded audit record.
	LineAlert
)

// ClassifyLine decodes one log line and says what it was. It is ParseLine plus
// the noise/malformed distinction the relay needs for its failure budget.
func ClassifyLine(line []byte) (Alert, LineKind) {
	if al, ok := ParseLine(line); ok {
		return al, LineAlert
	}
	if looksLikeAudit(line) {
		return Alert{}, LineMalformed
	}
	return Alert{}, LineNoise
}

// looksLikeAudit reports whether a line is shaped like one of the audit
// sources' records: a JSON object carrying a discriminator key (the engine's
// `Operation`, the egress proxy's `verdict`, the tool proxy's `tool`). Anything
// else is ordinary output and is skipped without touching the failure budget.
func looksLikeAudit(line []byte) bool {
	trimmed := strings.TrimSpace(string(line))
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	return strings.Contains(trimmed, `"Operation"`) ||
		strings.Contains(trimmed, `"verdict"`) ||
		strings.Contains(trimmed, `"tool"`)
}

// ParseLine decodes one log line from any of the audit sources. It is a
// TOLERANT decode: unknown keys are ignored (the engine gains fields between
// releases) and a line that is not an audit line at all — a non-JSON line, a
// truncated line, an unrelated log message — reports false so the caller can
// skip it. A line whose timestamp cannot be read is also rejected: the relay
// cannot order, dedupe or gap-check it without one.
func ParseLine(line []byte) (Alert, bool) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return Alert{}, false
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &raw); err != nil {
		return Alert{}, false
	}
	switch {
	// The KubeArmor alert JSON is PascalCase and always carries Operation.
	case has(raw, "Operation"):
		return parseKubeArmor(raw)
	// The egress-proxy record (internal/egress.AuditRecord): a Q4 envelope
	// without iteration, plus a verdict.
	case has(raw, "verdict"):
		return parseEgress(raw)
	// The tool-proxy record (internal/toolproxy.AuditRecord): the Q4 envelope
	// minus verdict, with a tool/status pair instead.
	case has(raw, "tool"), has(raw, "status"):
		return parseToolProxy(raw)
	}
	return Alert{}, false
}

// parseKubeArmor decodes the engine's alert JSON. Only the fields the relay
// needs are read (the design's "tolerant decode of a few fields"); every other
// key — there are ~25 in a real line, including ones added between KubeArmor
// releases — is ignored.
func parseKubeArmor(raw map[string]json.RawMessage) (Alert, bool) {
	ts, ok := kubeArmorTime(raw)
	if !ok {
		return Alert{}, false
	}
	action := str(raw, "Action")
	al := Alert{
		Time:      ts,
		Action:    action,
		Blocked:   strings.EqualFold(action, ActionBlock),
		Operation: str(raw, "Operation"),
		Namespace: str(raw, "NamespaceName"),
		Pod:       str(raw, "PodName"),
		Container: str(raw, "ContainerName"),
		Source:    SourceKubeArmor,
	}
	al.Target = firstNonEmpty(str(raw, "Resource"), str(raw, "Source"), str(raw, "Operation"))
	if policy := str(raw, "PolicyName"); policy != "" {
		al.Detail = "policy=" + policy
	}
	if data := str(raw, "Data"); data != "" {
		al.Detail = strings.TrimPrefix(al.Detail+", data="+data, ", ")
	}
	return al, true
}

// parseEgress decodes an egress-proxy audit record (internal/egress).
func parseEgress(raw map[string]json.RawMessage) (Alert, bool) {
	ts, ok := parseTime(str(raw, "time"))
	if !ok {
		return Alert{}, false
	}
	al := Alert{
		Time:          ts,
		Action:        str(raw, "action"),
		Operation:     str(raw, "action"),
		Loop:          str(raw, "loop"),
		LoopNamespace: str(raw, "namespace"),
		Namespace:     str(raw, "namespace"),
		Target:        str(raw, "target"),
		Source:        firstNonEmpty(str(raw, "source"), SourceEgress),
		Detail:        str(raw, "detail"),
	}
	al.Blocked = strings.EqualFold(str(raw, "verdict"), "blocked")
	return al, true
}

// parseToolProxy decodes a tool-proxy audit record (internal/toolproxy).
func parseToolProxy(raw map[string]json.RawMessage) (Alert, bool) {
	ts, ok := parseTime(str(raw, "time"))
	if !ok {
		return Alert{}, false
	}
	status, _ := strconv.Atoi(str(raw, "status"))
	al := Alert{
		Time:          ts,
		Action:        firstNonEmpty(str(raw, "action"), "request"),
		Operation:     "Tool",
		Loop:          str(raw, "loop"),
		LoopNamespace: str(raw, "namespace"),
		Namespace:     str(raw, "namespace"),
		Target:        strings.TrimSpace(str(raw, "method") + " " + str(raw, "path")),
		Source:        firstNonEmpty(str(raw, "source"), SourceToolProxy),
		Detail:        toolProxyDetail(raw, status),
	}
	// The tool proxy answers 403/405/400 for a refused request; any 4xx/5xx is
	// not an allowed call.
	al.Blocked = status >= 400
	return al, true
}

// toolProxyDetail renders the tool proxy's correlation data: the response
// status and the policy hash (the egress parser keeps its policy= the same way,
// so the two sources' detail fields carry the same kind of information).
func toolProxyDetail(raw map[string]json.RawMessage, status int) string {
	detail := "status=" + strconv.Itoa(status)
	if policy := str(raw, "policy"); policy != "" {
		detail += ", policy=" + policy
	}
	return detail
}

// kubeArmorTime reads the alert's own clock: UpdatedTime (RFC3339Nano) when
// present, else Timestamp (Unix seconds).
func kubeArmorTime(raw map[string]json.RawMessage) (time.Time, bool) {
	if ts, ok := parseTime(str(raw, "UpdatedTime")); ok {
		return ts, true
	}
	if s := str(raw, "Timestamp"); s != "" {
		if secs, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(secs, 0).UTC(), true
		}
	}
	return time.Time{}, false
}

// parseTime parses an RFC3339(Nano) timestamp.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts.UTC(), true
	}
	return time.Time{}, false
}

// str returns the string value of the first present key, "" when absent or not
// a string. Numbers are rendered as their literal text (the engine writes
// PIDs/exit codes as numbers, and the relay only needs the text).
func str(raw map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		v, ok := raw[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err == nil {
			return s
		}
		trimmed := strings.Trim(string(v), `"`)
		if trimmed != "" && trimmed != "null" {
			return trimmed
		}
	}
	return ""
}

// has reports whether the JSON object carries the key at all (the shape
// discriminator above must not treat an explicit null as absent).
func has(raw map[string]json.RawMessage, key string) bool {
	_, ok := raw[key]
	return ok
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
