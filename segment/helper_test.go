package segment

import (
	"fmt"
	"strings"
)

// benchSource builds a Go file with n top-level funcs, each with a doc
// comment, params and a body: the shape the walk actually visits.
func benchSource(n int) []byte {
	var b strings.Builder
	b.WriteString("package bench\n\nimport \"fmt\"\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `
// Fn%d does a thing worth documenting, at a length real doc comments reach.
type T%d struct {
	Name string
	Size int
}

func (t T%d) Method%d(ctx string, retries int) (string, error) {
	if retries > 0 {
		fmt.Println(ctx, t.Name)
	}
	return t.Name, nil
}

func Fn%d(a, b int) int {
	total := a + b
	for i := 0; i < b; i++ {
		total += i
	}
	return total
}
`, i, i, i, i, i)
	}
	return []byte(b.String())
}
