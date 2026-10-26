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

package main

import (
	"strings"
	"testing"
)

// F65 (supply chain P2-1/P2-2): the operator's three runtime-pulled DEFAULT
// images are pinned by tag + digest. A tag alone is mutable, and the operator
// pulls these on other people's clusters at run time; the workspace-git image
// is the one that carries the git credential (clone/deliver).
//
// Mutation: drop the "@sha256:..." suffix from any one of the three constants →
// this test fails (the design's named mutation).
func TestDefaultImagesArePinnedByDigest(t *testing.T) {
	pins := map[string]string{
		"--cni-probe-image":     pinnedCNIProbeImage,
		"--workspace-git-image": pinnedWorkspaceGitImage,
		"--verify-image":        pinnedVerifyImage,
	}
	for flag, ref := range pins {
		if !strings.Contains(ref, "@sha256:") {
			t.Errorf("%s default %q is not digest-pinned: "+
				"a mutable tag is pulled at run time on someone else's cluster", flag, ref)
		}
		// A digest pin must not be a floating tag either: ":latest" plus a
		// digest is a ref that reads like a moving one.
		if strings.Contains(ref, ":latest@") {
			t.Errorf("%s default %q pins a :latest tag; use a version tag with the digest",
				flag, ref)
		}
	}
	// The probe image moved off the floating ":3-alpine" minor tag, so the
	// interpreter version cannot change under the operator between two probes.
	if !strings.Contains(pinnedCNIProbeImage, "python:3.13-alpine@sha256:") {
		t.Errorf("cni probe image = %q, want a version-exact python:3.13-alpine with a digest",
			pinnedCNIProbeImage)
	}
}

// TestDefaultImageDigestsAreExact (early-323 N6) spells the three refs out in
// full. The shape checks above stay green for a digest with one wrong character,
// and every pull then fails at run time — the exact failure this pin exists to
// prevent. With the literals written out twice, changing a pin is a deliberate
// two-place edit.
//
// Mutation: change any one hex digit in a constant above → this test fails.
func TestDefaultImageDigestsAreExact(t *testing.T) {
	want := []struct {
		flag string
		ref  string
	}{
		{"--cni-probe-image", "docker.io/library/python:3.13-alpine@sha256:" +
			"bf12ecc0bea3f171d55145dd02c78b5eeb09f30ee2ac9cc2b4c9e4e8f5e30741"},
		{"--workspace-git-image", "docker.io/alpine/git:v2.54.0@sha256:" +
			"e8ceedc73bd80992ae8d58a7d24c58b8322146c354f1d2e81a97db4452068ec0"},
		{"--verify-image", "docker.io/library/golang:1.26@sha256:" +
			"672c944cfd07599b5f1bb179869270cdc94c6254e41db4d431259693cb286b82"},
	}
	got := map[string]string{
		"--cni-probe-image":     pinnedCNIProbeImage,
		"--workspace-git-image": pinnedWorkspaceGitImage,
		"--verify-image":        pinnedVerifyImage,
	}
	for _, tc := range want {
		if got[tc.flag] != tc.ref {
			t.Errorf("%s default = %q, want %q", tc.flag, got[tc.flag], tc.ref)
		}
	}
}
