//go:build cgo && treesitter_cgo

package segment

import (
	"strings"
	"testing"
)

func TestRuntimeCgoTag(t *testing.T) {
	if got := Runtime(); got != "smacker-cgo" && !strings.HasPrefix(got, "smacker-cgo@") {
		t.Errorf("Runtime() = %q, want smacker-cgo[@version]", got)
	}
}
