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
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// sandboxName is now simply <loop>-sandbox. The guarantee that the result is a
// valid DNS-1035 label <= 63 chars comes from the CRD's CEL validation on the
// Loop name (D20: max 55 chars), not from sandboxName. These tests pin the
// trivial behaviour.
var _ = Describe("sandboxName", func() {
	It("is <name>-sandbox", func() {
		Expect(sandboxName("smoke")).To(Equal("smoke-sandbox"))
		Expect(sandboxName("test-resource")).To(Equal("test-resource-sandbox"))
	})

	It("stays within 63 chars for a 55-char (the max valid) Loop name", func() {
		name := strings.Repeat("a", 55)
		got := sandboxName(name)
		Expect(got).To(HaveLen(55 + len("-sandbox")))
		Expect(len(got)).To(BeNumerically("<=", 63))
	})
})
