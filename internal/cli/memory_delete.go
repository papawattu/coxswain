package cli

// I101: `cox memory delete <repo-url>` removes the per-(namespace, repo) memory
// PVC. The PVC deliberately outlives its Loops (the lessons are the point), so
// there was no supported way to get a clean slate. Delete refuses while a
// non-terminal Loop on the same repo exists in the namespace: that Loop's
// agent pod mounts the PVC and its writer Job may still add lessons.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/papawattu/coxswain/pkg/memkey"
)

// MemoryAdmin is the `memory delete` boundary: list the Loops in the
// namespace and delete a PVC. Tests stub it; nil = real kubectl.
type MemoryAdmin interface {
	// ActiveLoops returns "name (phase)" for every Loop in the namespace on
	// repoURL whose phase is not terminal (Succeeded or Failed; an unset phase
	// is not terminal).
	ActiveLoops(ctx context.Context, repoURL string) ([]string, error)
	// DeletePVC deletes the named PVC. A missing PVC is memoryNotFoundErr.
	DeletePVC(ctx context.Context, name string) error
}

func newMemoryDeleteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <repo-url>",
		Short: "Delete the repo's shared memory PVC (refused while a Loop on the repo is active)",
		Long: "cox memory delete removes the memory PVC for <repo-url> in the current\n" +
			"namespace, discarding every lesson in it. Memory PVCs persist after their\n" +
			"Loops are deleted, so this is the supported way to start a repo's memory\n" +
			"from scratch. It refuses while a Loop on the repo that is not Succeeded or\n" +
			"Failed exists in the namespace. Export first (`cox memory export`) to keep a\n" +
			"copy.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return memoryDelete(cmd.Context(), currentDeps, namespaceOf(), args[0], cmd.OutOrStdout())
		},
	}
}

func memoryDelete(ctx context.Context, dc Deps, ns, repoURL string, out io.Writer) error {
	admin := dc.MemoryAdmin
	if admin == nil {
		admin = &kubectlMemoryAdmin{}
	}
	name := memkey.PVCName(repoURL)
	if name == "" {
		return fmt.Errorf("an empty repo URL has no memory PVC")
	}
	active, err := admin.ActiveLoops(ctx, repoURL)
	if err != nil {
		return fmt.Errorf("list Loops in namespace %s: %w", ns, err)
	}
	if len(active) > 0 {
		slices.Sort(active)
		return fmt.Errorf("refusing to delete memory PVC %s: %d active Loop(s) on %s in namespace %s: %s "+
			"(wait for them to reach Succeeded or Failed, or delete them)",
			name, len(active), repoURL, ns, strings.Join(active, ", "))
	}
	if err := admin.DeletePVC(ctx, name); err != nil {
		if isNoMemoryErr(err) {
			_, werr := fmt.Fprintf(out, "No memory PVC %s in namespace %s; nothing to delete.\n", name, ns)
			return werr
		}
		return fmt.Errorf("delete memory PVC %s in namespace %s: %w", name, ns, err)
	}
	_, err = fmt.Fprintf(out, "Deleted memory PVC %s in namespace %s (repo %s).\n", name, ns, repoURL)
	return err
}

// kubectlMemoryAdmin is the real MemoryAdmin (kubectl, with the CLI's
// --kubeconfig/--context/--namespace globals).
type kubectlMemoryAdmin struct{}

func (kubectlMemoryAdmin) ActiveLoops(ctx context.Context, repoURL string) ([]string, error) {
	full := append(memoryKubeArgs(), "get", "loops.coxswain.wattu.com", "-o", "json")
	out, err := exec.CommandContext(ctx, "kubectl", full...).Output()
	if err != nil {
		return nil, err
	}
	return activeLoopsFromJSON(out, repoURL)
}

// activeLoopsFromJSON picks the non-terminal Loops on repoURL out of a
// `kubectl get loops -o json` List. The repo is matched exactly, as memkey
// keys it.
func activeLoopsFromJSON(data []byte, repoURL string) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				Workspace struct {
					Repo string `json:"repo"`
				} `json:"workspace"`
			} `json:"spec"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode Loop list: %w", err)
	}
	var active []string
	for _, it := range list.Items {
		if it.Spec.Workspace.Repo != repoURL {
			continue
		}
		if p := it.Status.Phase; p == "Succeeded" || p == "Failed" {
			continue
		}
		phase := it.Status.Phase
		if phase == "" {
			phase = loopPhaseUnset
		}
		active = append(active, fmt.Sprintf("%s (%s)", it.Metadata.Name, phase))
	}
	return active, nil
}

func (kubectlMemoryAdmin) DeletePVC(ctx context.Context, name string) error {
	full := append(memoryKubeArgs(), "delete", "pvc", name)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		if strings.Contains(stderr.String(), "NotFound") {
			return memoryNotFoundErr{}
		}
		fmt.Fprint(os.Stderr, stderr.String())
		return err
	}
	return nil
}

// loopPhaseUnset names a Loop the operator has not reconciled yet.
const loopPhaseUnset = "Pending"
