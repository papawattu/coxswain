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
// and exits 0 once two consecutive connects FAIL: refused (kube-router
// REJECTs) or timed out (a DROP). The kubelet listens on every node and the
// DNS-only probe policy does not allow it, so the connect succeeds before the
// fence is up and fails after. If the budget runs out first, it exits with
// fenceWaitTimeoutExit, and the probe reports CNIUnenforced: the kubelet
// stayed reachable under the policy for the whole budget.
//
// fence-wait only decides WHEN the measurement starts, so it cannot cause a
// false CNIEnforced. If it passes early (say, the kubelet happens to refuse
// for another reason), the probe still runs its full measurement, and a leak
// still reads REACHABLE (fail-closed). The control pass runs with no policy
// and does NOT fence-wait: it would never see a fence and would always time
// out.
package cni

import (
	"errors"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	fenceWaitContainerName = "fence-wait"
	// fenceWaitTimeoutExit is the exit code for "budget exhausted, the fence
	// never came up". Any other non-zero exit is a crash, not a verdict.
	fenceWaitTimeoutExit = 2
	// DefaultFenceWaitBudget is the default --cni-fence-wait-budget.
	DefaultFenceWaitBudget = 30 * time.Second
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
// probe container's image, pull policy, NODE_IP env and security context, so
// it adds no new image and no new privilege.
func (p *PodProber) fenceWaitContainer(probe corev1.Container) corev1.Container {
	env := append([]corev1.EnvVar{}, probe.Env...)
	env = append(env, corev1.EnvVar{
		Name:  "FENCE_WAIT_BUDGET_SECONDS",
		Value: strconv.FormatFloat(p.fenceWaitBudget().Seconds(), 'f', -1, 64),
	})
	return corev1.Container{
		Name:                     fenceWaitContainerName,
		Image:                    probe.Image,
		ImagePullPolicy:          probe.ImagePullPolicy,
		Command:                  fenceWaitCommand(),
		Env:                      env,
		TerminationMessagePath:   "/dev/termination-log",
		TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
		SecurityContext:          probe.SecurityContext.DeepCopy(),
	}
}

func fenceWaitCommand() []string {
	return []string{"python3", "-c", `
import os, socket, sys, time

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
    except Exception:
        blocked_in_a_row += 1
    finally:
        s.close()
    elapsed = time.monotonic() - t0
    if blocked_in_a_row >= 2:
        report("FENCE_UP elapsed=%.2f attempts=%d" % (elapsed, attempts))
        sys.exit(0)
    if elapsed >= budget:
        report("FENCE_TIMEOUT elapsed=%.2f attempts=%d" % (elapsed, attempts))
        sys.exit(` + strconv.Itoa(fenceWaitTimeoutExit) + `)
    time.sleep(0.1)
`}
}

// fenceWaitStatus reads the fence-wait init container's terminated state off
// a pod, or nil if it has not terminated (or the pod has no fence-wait).
func fenceWaitStatus(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == fenceWaitContainerName {
			return cs.State.Terminated
		}
	}
	return nil
}
