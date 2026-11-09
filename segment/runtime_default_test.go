//go:build !(cgo && treesitter_cgo)

package segment

import (
	"strings"
	"testing"
)

func TestRuntimeDefaultIsPureGo(t *testing.T) {
	if got := Runtime(); got != "gotreesitter" && !strings.HasPrefix(got, "gotreesitter@") {
		t.Errorf("Runtime() = %q, want gotreesitter[@version]", got)
	}
}
