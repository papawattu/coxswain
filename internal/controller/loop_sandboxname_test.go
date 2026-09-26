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

package controller

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// P3 tidy-up: sandboxName keeps the Loop's sandbox name within the 63-char k8s
// limit. Short Loop names pass through; long ones are hash-truncated.
var _ = Describe("sandboxName", func() {
	It("passes short names through unchanged", func() {
		Expect(sandboxName("smoke")).To(Equal("smoke-sandbox"))
		Expect(sandboxName("test-resource")).To(Equal("test-resource-sandbox"))
	})

	It("keeps the result within 63 chars for long names", func() {
		long := ""
		for range 80 {
			long += "x"
		}
		got := sandboxName(long)
		Expect(len(got)).To(BeNumerically("<=", 63))
	})

	It("is deterministic and distinct per name", func() {
		a := sandboxName("a-very-long-loop-name-that-would-overflow-the-63-char-limit-abcdef")
		b := sandboxName("a-very-long-loop-name-that-would-overflow-the-63-char-limit-abcdef")
		c := sandboxName("a-very-long-loop-name-that-would-overflow-the-63-char-limit-XYZZYZ")
		Expect(a).To(Equal(b), "same name must produce the same sandbox name")
		Expect(a).ToNot(Equal(c), "different names must produce different sandbox names")
	})
})
