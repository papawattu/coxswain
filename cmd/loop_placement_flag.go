// C2 + D101 (Phase 6, PLAN-PHASE6.md C2, owner decision 2026-11-01): the
// operator's Loop node-placement flags. --loop-node-selector is a
// comma-separated key=value list; --loop-tolerations is a comma-separated
// key:op:effect[:value[:seconds]] list (the approved plan's format,
// PLAN-PHASE6.md:101-102, where the op field is ALWAYS field 2, so a token
// is never ambiguous). A malformed token is a STARTUP error that names the
// bad token — never a silent drop (a silently dropped token means the
// operator comes up with a different placement than configured). Empty flag
// = no placement (today's behaviour). The parsing helpers are NON-test
// sources: they compile into the operator binary, and
// loop_placement_flag_test.go pins the malformed-token contract (a helper
// that silently dropped a bad token fails that test).
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
	for tok := range strings.SplitSeq(raw, ",") {
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

// parseLoopTolerations parses the --loop-tolerations flag using the
// approved plan's format (PLAN-PHASE6.md:101-102):
//
//	key:op:effect[:value[:seconds]]
//
// where op is ALWAYS field 2 (so a token can never be misread) and is
// `Equal` or `Exists` (NotEqual is not a Kubernetes toleration operator and
// is rejected), and effect is `NoSchedule`, `PreferNoSchedule` or
// `NoExecute`. Field count decides the shape — 3 is bare
// (key:op:effect), 4 carries a value, 5 carries value AND seconds:
//
//	key:Equal:NoSchedule        -> {Key, Equal, Effect}
//	key:Equal:NoSchedule:loops  -> {Key, Equal, Value, Effect}
//	key:Exists:NoExecute::30    -> {Key, Exists, Effect, TolerationSeconds}
//	key:Equal:NoSchedule:loops:120
//
// `Equal` REQUIRES a value; `Exists` must NOT have one (so a seconds-only
// Exists token keeps the value slot empty: key:Exists:NoExecute::30 —
// seconds sits in the field after the empty value). A value with `Exists`,
// a missing value with `Equal`, a seconds field that is not a non-negative
// integer, a bad operator or effect, or a field count outside 3-5 is a
// STARTUP error naming the token. A blank token (double comma) is a blank,
// not a malformed token.
func parseLoopTolerations(raw string) ([]corev1.Toleration, error) {
	var out []corev1.Toleration
	for tok := range strings.SplitSeq(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		tol, err := parseLoopTolerationToken(tok)
		if err != nil {
			return nil, err
		}
		out = append(out, tol)
	}
	return out, nil
}

// parseLoopTolerationToken parses one non-blank --loop-tolerations token
// (see parseLoopTolerations for the full contract). Extracted per-token so
// the caller stays a flat loop.
func parseLoopTolerationToken(tok string) (corev1.Toleration, error) {
	f := strings.Split(tok, ":")
	if len(f) < 3 || len(f) > 5 {
		return corev1.Toleration{}, fmt.Errorf(
			"--loop-tolerations token %q is malformed (want key:op:effect, "+
				"key:op:effect:value or key:op:effect:value:seconds)", tok)
	}
	if f[0] == "" || f[1] == "" || f[2] == "" {
		return corev1.Toleration{}, fmt.Errorf(
			"--loop-tolerations token %q is malformed (key, op and effect must be non-empty)", tok)
	}
	op, err := loopTolerationOperator(f[1])
	if err != nil {
		return corev1.Toleration{}, fmt.Errorf("--loop-tolerations token %q is malformed: %v", tok, err)
	}
	effect, err := loopTolerationEffect(f[2])
	if err != nil {
		return corev1.Toleration{}, fmt.Errorf("--loop-tolerations token %q is malformed: %v", tok, err)
	}
	value := ""
	if len(f) >= 4 {
		value = f[3]
	}
	var seconds *int64
	if len(f) == 5 {
		if f[4] == "" {
			return corev1.Toleration{}, fmt.Errorf(
				"--loop-tolerations token %q is malformed (the seconds field is empty in a 5-field token)", tok)
		}
		si, err := strconv.Atoi(f[4])
		if err != nil || si < 0 {
			return corev1.Toleration{}, fmt.Errorf(
				"--loop-tolerations token %q is malformed (the seconds field %q is not a non-negative integer)", tok, f[4])
		}
		v := int64(si)
		seconds = &v
	}
	switch op {
	case corev1.TolerationOpEqual:
		if value == "" {
			return corev1.Toleration{}, fmt.Errorf(
				"--loop-tolerations token %q is malformed (op Equal requires a value)", tok)
		}
	case corev1.TolerationOpExists:
		if value != "" {
			return corev1.Toleration{}, fmt.Errorf(
				"--loop-tolerations token %q is malformed (op Exists must not carry a value)", tok)
		}
	}
	return corev1.Toleration{
		Key: f[0], Operator: op, Value: value,
		Effect: effect, TolerationSeconds: seconds,
	}, nil
}

// loopTolerationOperator maps the op field (always field 2) to a k8s
// toleration operator. Only Equal and Exists are valid — NotEqual is NOT a
// Kubernetes toleration operator, so it is rejected (a malformed token is a
// startup error, not a guess).
func loopTolerationOperator(s string) (corev1.TolerationOperator, error) {
	switch s {
	case "Equal":
		return corev1.TolerationOpEqual, nil
	case "Exists":
		return corev1.TolerationOpExists, nil
	default:
		return "", fmt.Errorf(
			"the operator %q is not one of Equal, Exists (NotEqual is not a Kubernetes toleration operator)", s)
	}
}

// loopTolerationEffect maps the effect field (always field 3) to a k8s
// taint effect. Only the three real taint effects are accepted.
func loopTolerationEffect(s string) (corev1.TaintEffect, error) {
	switch s {
	case "NoSchedule":
		return corev1.TaintEffectNoSchedule, nil
	case "PreferNoSchedule":
		return corev1.TaintEffectPreferNoSchedule, nil
	case "NoExecute":
		return corev1.TaintEffectNoExecute, nil
	default:
		return "", fmt.Errorf("the effect %q is not one of NoSchedule, PreferNoSchedule, NoExecute", s)
	}
}
