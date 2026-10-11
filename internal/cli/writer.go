package cli

import (
	"fmt"
	"io"
)

// writer is a thin stdout wrapper that tracks the first error so commands can
// return it after rendering.
type writer struct {
	w      io.Writer
	fail   error
}

func newWriter(w io.Writer) *writer { return &writer{w: w} }

func (x *writer) printf(format string, a ...any) {
	if x.fail != nil {
		return
	}
	if _, err := io.WriteString(x.w, fmt.Sprintf(format, a...)); err != nil {
		x.fail = err
	}
}

func (x *writer) err() error { return x.fail }

// splitLines splits a string on newlines (a small helper for the condition
// messages; avoids importing strings in the render path).
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
