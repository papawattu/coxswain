// C2 + D101 (Phase 6, PLAN-PHASE6.md C2, owner decision 2026-11-01): the
// operator's Loop node-placement flags. --loop-node-selector is a
// comma-separated key=value list; --loop-tolerations is a comma-separated
// key:op:effect[:value[:seconds]] list (the documented forms, in which the
// EFFECT is the first field, so the operator can build the flag from
// `kubectl taint node <n> <key>:<value>:<effect>` almost verbatim). A
// malformed token is a STARTUP error that names the bad token — never a
// silent drop (a silently dropped token means the operator comes up with a
// different placement than configured). Empty flag = no placement
// (today's behaviour). The parsing helpers are NON-test sources: they
// compile into the operator binary, and loop_placement_flag_test.go pins
// the malformed-token contract (a helper that silently dropped a bad token
// fails that test).
package main

import (
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// parseLoopNodeSelector parses the --loop-node-selector flag. Empty (or
// whitespace/blank-only) input is a no placement (the map is empty), not an
// error. Each comma-separated token must be `key=value`: a non-empty key, a
// non-empty value, exactly one `=`. A token that is only whitespace (e.g. a
// double comma) is a blank, not a malformed token.
func parseLoopNodeSelector(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.Split(tok, "=")
		if len(parts) != 2 {
			return nil, fmt.Errorf("--loop-node-selector token %q is malformed (want key=value)", tok)
		}
		key, val := parts[0], parts[1]
		if key == "" || val == "" {
			return nil, fmt.Errorf("--loop-node-selector token %q is malformed (key and value must be non-empty)", tok)
		}
		out[key] = val
	}
	return out, nil
}

// parseLoopTolerations parses the --loop-tolerations flag. The accepted
// forms (the EFFECT is always the first field):
//
//	key:effect                    -> {Key, Operator: Equal, Effect}
//	key:value:effect              -> {Key, Operator: Equal, Value, Effect}
//	key:value:op:effect           -> {Key, Operator: op, Value, Effect} (op: Equal | Exists | NotEqual)
//	key:value:op:effect:seconds   -> as above + TolerationSeconds
//
// A trailing field after a two-field token is the seconds form
// (key:effect:seconds). The shape is decided by FIELD COUNT, so the kind of
// the middle field is never guessed: key:value:NoSchedule cannot parse as a
// seconds form (seconds is an integer) and cannot parse as an op form
// (NoSchedule is not an operator). Seconds must be a non-negative integer;
// an operator must be one of the k8s operator constants. A malformed token
// is a STARTUP error naming the token; a blank token (double comma) is a
// blank, not a malformed token.
func parseLoopTolerations(raw string) ([]corev1.Toleration, error) {
	var out []corev1.Toleration
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		f := strings.Split(tok, ":")
		switch len(f) {
		case 2:
			if f[0] == "" || f[1] == "" {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (want key:effect)", tok)
			}
			out = append(out, corev1.Toleration{Key: f[0], Operator: corev1.TolerationOpEqual, Effect: corev1.TolerationEffect(f[1])})
		case 3:
			// key:effect:seconds (the two-field form plus a trailing seconds
			// value). A token whose middle field is a VALUE (not an effect +
			// an int) fails here on the parse — no kind-guessing.
			if f[0] == "" || f[1] == "" || f[2] == "" {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (want key:effect[:seconds])", tok)
			}
			secs, err := strconv.Atoi(f[2])
			if err != nil || secs < 0 {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (the seconds field %q is not a non-negative integer)", tok, f[2])
			}
			out = append(out, corev1.Toleration{Key: f[0], Operator: corev1.TolerationOpEqual, Effect: corev1.TolerationEffect(f[1]), TolerationSeconds: &secs})
		case 4:
			// key:value:op:effect (the full form; the op is an explicit
			// operator, not Equal-by-default).
			if f[0] == "" || f[1] == "" || f[2] == "" || f[3] == "" {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (want key:value:op:effect)", tok)
			}
			op, err := loopTolerationOperator(f[2])
			if err != nil {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed: %v", tok, err)
			}
			out = append(out, corev1.Toleration{Key: f[0], Value: f[1], Operator: op, Effect: corev1.TolerationEffect(f[3])})
		case 5:
			// key:value:op:effect:seconds.
			if f[0] == "" || f[1] == "" || f[2] == "" || f[3] == "" || f[4] == "" {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (want key:value:op:effect[:seconds])", tok)
			}
			op, err := loopTolerationOperator(f[2])
			if err != nil {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed: %v", tok, err)
			}
			secs, err := strconv.Atoi(f[4])
			if err != nil || secs < 0 {
				return nil, fmt.Errorf("--loop-tolerations token %q is malformed (the seconds field %q is not a non-negative integer)", tok, f[4])
			}
			out = append(out, corev1.Toleration{Key: f[0], Value: f[1], Operator: op, Effect: corev1.TolerationEffect(f[3]), TolerationSeconds: &secs})
		default:
			return nil, fmt.Errorf("--loop-tolerations token %q is malformed (want key:effect, key:value:effect, key:value:op:effect or key:value:op:effect:seconds)", tok)
		}
	}
	return out, nil
}

// loopTolerationOperator maps the op field to a k8s operator (only the
// Equal/Exists/NotEqual forms are accepted — the effect forms are covered
// by the two-field token; a bare `Equal` in the op slot of the four-field
// form is valid but unusual).
func loopTolerationOperator(s string) (corev1.TolerationOperator, error) {
	switch s {
	case "Equal":
		return corev1.TolerationOpEqual, nil
	case "Exists":
		return corev1.TolerationOpExists, nil
	case "NotEqual":
		return corev1.TolerationOpNotEqual, nil
	default:
		return "", fmt.Errorf("the operator %q is not one of Equal, Exists, NotEqual", s)
	}
}
