# D38 probe EXTERNAL positive control is structurally broken under the DNS-only netpol

## Context
D38 (operator CNI self-test) probe on the coxswain-calico kind cluster (enforcing
CNI = Calico, no KubeArmor by design ADR-0007 F2). The operator runs a
`python:3-alpine` probe pod (namespace `coxswain-cni-probe`, label
`coxswain.io/probe=cni-probe`) under a probe NetworkPolicy, reads RESULT lines
from the pod's termination message, and sets each Loop's
`NetworkEnforced` condition accordingly (CNIEnforced / CNIUnenforced /
ProbeUnavailable / Unknown).

## Problem
The probe NetworkPolicy (internal/cni/pod_prober.go ensureNetpol) allows egress
ONLY to kube-system kube-dns on 53 UDP+TCP (the verify-cni.sh 1:1 shape —
deliberately changed away from the old agent-shaped 0.0.0.0/0-minus-RFC1918
allow list). So the probe pod has NO external egress.

But parse() (internal/cni/pod_prober.go, ~line 479-490) requires the EXTERNAL
positive control (1.1.1.1:443) to come back REACHABLE:

    if any row.Label == "EXTERNAL" && row.Verdict == verdictReachable: pass
    else: return unavailable("EXTERNAL control not REACHABLE: the probe has no network; the fence is unproven")

Under a DNS-only netpol, 1.1.1.1:443 CANNOT be REACHABLE (no egress to
1.1.1.1). So the EXTERNAL control is BLOCKED -> parse returns ProbeUnavailable
-> every Loop is held Suspended with NetworkEnforced=False ProbeUnavailable.
The D38 gate can NEVER pass on an enforcing CNI. The positive control is
structurally incompatible with the netpol that defines the fence.

## What a "positive control" needs to be valid
A positive control (REACHABLE-expected target) only proves the probe has a
working network stack if the netpol ALLOWS that egress. Options:
- (a) Add an egress rule to the probe netpol that allows the EXTERNAL target
      (e.g. IPBlock 1.1.1.1/32:443, or 0.0.0.0/0 excluding the blocked
      targets) so 1.1.1.1:443 is legitimately REACHABLE. Risk: must not
      accidentally allow the expected-BLOCKED targets (apiserver svc ClusterIP,
      node :6443/:10250) or the C1-C3 assertions go false.
- (b) Drop the EXTERNAL positive control entirely and rely on the
      expected-BLOCKED targets (C1-C3) + the probe pod's own DNS resolution
      (it must resolve node_ip via the downward API, and the probe netpol
      allows DNS — if DNS worked, the probe has a network path).
- (c) Redefine "the probe has a network" by checking that DNS itself works
      (a DNS query to kube-dns succeeds) rather than a TCP connect to an
      external IP.

## Verification (on coxswain-calico, image coxswain-controller:main-ea2c215)
- Syntax fix (pod_prober.go:166 added the opening quote before the first
  backtick so the python tuple is ("APISERVER_SVC", "kubernetes.default.svc.
  cluster.local", 443)) is VERIFIED WORKING: the probe pod now runs and emits
  RESULT lines (no more "unrecognised probe line: File <string>, line 14").
- After the fix, d38-loop NetworkEnforced = False ProbeUnavailable with
  message "EXTERNAL control not REACHABLE: the probe has no network; the fence
  is unproven" (was: "unrecognised probe line ... line 14" before the fix).
- netpol confirmed: egress = [DNS 53 UDP+TCP to kube-system kube-dns], no
  IPBlock. So 1.1.1.1:443 has no allowed egress -> BLOCKED is correct -> the
  gate can never pass until the EXTERNAL control design is fixed.

## Files
- internal/cni/pod_prober.go (ensureNetpol ~line 293, parse ~line 439-490,
  probeCommand ~line 151)
- test/e2e/d38-cni-e2e.sh (step 3c asserts NetworkEnforced=True CNIEnforced)

## Decision needed
How to make the EXTERNAL positive control valid (option a/b/c above), or
remove it. This is a design decision for the reviewer/owner — it changes what
the probe netpol allows and what parse() requires.
