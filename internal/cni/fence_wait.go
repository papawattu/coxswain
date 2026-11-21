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

// fence-wait: wait for the pod's own NetworkPolicy to take effect before the
// probe measures it.
//
// On k3s, the embedded kube-router programs a NEW pod's NetworkPolicy rules
// asynchronously, AFTER its container starts. This is true even when the
// policy already exists. Measured on the homelab: 0.3-1.2s on 2026-11-07 and
// 0.05-0.26s on 2026-11-21, on every node. The policy pass connected at t=0,
// measured inside that window, and reported CNIUnenforced on a cluster that
// does enforce. Calico's Felix has the same window unless the CNI plugin's
// policy_setup_timeout_seconds is set.
//
// The policy pass now starts with a fence-wait init container. It connects
// to the node's kubelet (NODE_IP:10250, the KUBELET_NODE target) every 100ms,
// and exits 0 once two consecutive connects FAIL the way a NetworkPolicy
// fails them: refused (kube-router REJECTs; EHOSTUNREACH for an
// icmp-host-prohibited REJECT) or timed out (a DROP). Any other connect error
// (EPERM from an LSM such as KubeArmor, ENETUNREACH) is not a fence and resets
// the count. The kubelet listens on every node and the
// DNS-only probe policy does not allow it, so the connect succeeds before the
// fence is up and fails after. If the budget runs out first, it exits with
// FenceWaitTimeoutExit, and the probe reports CNIUnenforced: the kubelet
// stayed reachable under the policy for the whole budget.
//
// fence-wait only decides WHEN the measurement starts, so it cannot cause a
// false CNIEnforced. If it passes early (say, the kubelet happens to refuse
// for another reason), the probe still runs its full measurement, and a leak
// still reads REACHABLE (fail-closed). The control pass runs with no policy
// and does NOT fence-wait: it would never see a fence and would always time
// out.
//
// The Loop sandbox pod runs the same container (NewFenceWaitContainer) as its
// first init container, so the clone and the agent start fenced. There, no
// measurement follows, which is why only NetworkPolicy-shaped errors count.
package cni

import (
	"errors"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// FenceWaitContainerName is the fence-wait init container's name, in the
	// probe's policy pass and in the Loop sandbox pod.
	FenceWaitContainerName = "fence-wait"
	// FenceWaitTimeoutExit is the exit code for "budget exhausted, the fence
	// never came up". Any other non-zero exit is a crash, not a verdict.
	FenceWaitTimeoutExit = 2
	// DefaultFenceWaitBudget is the default --cni-fence-wait-budget.
	DefaultFenceWaitBudget = 30 * time.Second
	// PinnedProbeImage is the default --cni-probe-image, and the image the
	// sandbox's fence-wait runs (tag + digest: the probe pod runs on every
	// node, and a moving tag would change what runs there unannounced).
	PinnedProbeImage = "docker.io/library/python:3.13-alpine@sha256:" +
		"bf12ecc0bea3f171d55145dd02c78b5eeb09f30ee2ac9cc2b4c9e4e8f5e30741"
)

// errFenceTimeout: fence-wait exhausted its budget with the kubelet still
// reachable under the probe policy.
var errFenceTimeout = errors.New("fence-wait budget exhausted: the NetworkPolicy never took effect")

func (p *PodProber) fenceWaitBudget() time.Duration {
	if p.cfg.FenceWaitBudget > 0 {
		return p.cfg.FenceWaitBudget
	}
	return DefaultFenceWaitBudget
}

// fenceWaitContainer is the policy pass's initContainers[0]. It reuses the
// probe container's image, pull policy and security context, so it adds no new
// image and no new privilege.
func (p *PodProber) fenceWaitContainer(probe corev1.Container) corev1.Container {
	return NewFenceWaitContainer(probe.Image, probe.ImagePullPolicy, p.fenceWaitBudget(), probe.SecurityContext)
}

// NewFenceWaitContainer builds a fence-wait init container: it runs image
// (which must provide python3), gets NODE_IP from the downward API
// (status.hostIP), and waits up to budget for the pod's own NetworkPolicy to
// refuse the node's kubelet. sc is deep-copied. A non-positive budget means
// DefaultFenceWaitBudget.
//
// It must be the pod's FIRST init container, so that nothing in the pod,
// including the other init containers, runs before the fence is up.
func NewFenceWaitContainer(image string, pull corev1.PullPolicy, budget time.Duration, sc *corev1.SecurityContext) corev1.Container {
	if budget <= 0 {
		budget = DefaultFenceWaitBudget
	}
	return corev1.Container{
		Name:            FenceWaitContainerName,
		Image:           image,
		ImagePullPolicy: pull,
		Command:         fenceWaitCommand(),
		Env: []corev1.EnvVar{
			{
				Name: "NODE_IP",
				ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.hostIP"},
				},
			},
			{
				Name:  "FENCE_WAIT_BUDGET_SECONDS",
				Value: strconv.FormatFloat(budget.Seconds(), 'f', -1, 64),
			},
		},
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          sc.DeepCopy(),
	}
}

func fenceWaitCommand() []string {
	return []string{"python3", "-c", `
import errno, os, socket, sys, time

# A fence refuses (REJECT: ECONNREFUSED, or EHOSTUNREACH for an
# icmp-host-prohibited REJECT) or drops (a timeout).
FENCED_ERRNOS = {errno.ECONNREFUSED, errno.ETIMEDOUT, errno.EHOSTUNREACH}
seen_other = set()

node_ip = os.environ.get("NODE_IP")
if not node_ip:
    sys.stderr.write("FATAL: NODE_IP not set (downward API status.hostIP missing)\n")
    sys.exit(3)
budget = float(os.environ.get("FENCE_WAIT_BUDGET_SECONDS", "30"))

def report(line):
    print(line, flush=True)
    try:
        with open("/dev/termination-log", "w") as f:
            f.write(line + "\n")
    except Exception:
        pass

t0 = time.monotonic()
attempts = 0
blocked_in_a_row = 0
while True:
    attempts += 1
    s = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    s.settimeout(0.5)
    try:
        s.connect((node_ip, 10250))
        blocked_in_a_row = 0
    except (ConnectionRefusedError, socket.timeout):
        blocked_in_a_row += 1
    except OSError as e:
        if e.errno in FENCED_ERRNOS:
            blocked_in_a_row += 1
        else:
            # Not a NetworkPolicy verdict (for example EPERM from an LSM, or
            # ENETUNREACH): it must not pass the fence.
            blocked_in_a_row = 0
            if e.errno not in seen_other:
                seen_other.add(e.errno)
                print("fence-wait: ignoring connect error %s (not a fence)" % e, flush=True)
    finally:
        s.close()
    elapsed = time.monotonic() - t0
    if blocked_in_a_row >= 2:
        report("FENCE_UP elapsed=%.2f attempts=%d" % (elapsed, attempts))
        sys.exit(0)
    if elapsed >= budget:
        report("FENCE_TIMEOUT elapsed=%.2f attempts=%d" % (elapsed, attempts))
        sys.exit(` + strconv.Itoa(FenceWaitTimeoutExit) + `)
    time.sleep(0.1)
`}
}

// FenceWaitStatus reads the fence-wait init container's terminated state off
// a pod, or nil if it has not terminated (or the pod has no fence-wait).
func FenceWaitStatus(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == FenceWaitContainerName {
			return cs.State.Terminated
		}
	}
	return nil
}
