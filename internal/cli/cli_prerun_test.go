/*
Copyright 2026 papawattu.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// R52 D121: a deps build failure (bad kubeconfig) must surface as the command's
// error from the pre-run hook, not be printed and then run the command with
// nil deps (which panics or has its own error silenced).
func TestPreRunReturnsKubeconfigError(t *testing.T) {
	oldDeps := currentDeps
	currentDeps = Deps{}
	t.Cleanup(func() { currentDeps = oldDeps })

	var out, errBuf bytes.Buffer
	root := NewRootCommand(Deps{})
	// The persistent flags bind into the package vars (Kubeconfig/Context),
	// so restore them: they leak into kubectl argv (and the client config)
	// in other tests in this package.
	oldKube, oldCtx := Kubeconfig, Context
	t.Cleanup(func() { Kubeconfig, Context = oldKube, oldCtx })
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"describe", "l1", "--namespace", "ns", "--kubeconfig", "/nonexistent/kubeconfig"})
	err := func() (e error) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic: %v", r)
			}
		}()
		return root.Execute()
	}()
	if err == nil {
		t.Fatal("Execute() = nil, want an error")
	}
	for _, want := range []string{"could not load the Kubernetes client config", "/nonexistent/kubeconfig"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
}
