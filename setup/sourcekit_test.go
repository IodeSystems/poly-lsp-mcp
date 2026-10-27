package setup

import (
	"strings"
	"testing"
)

func TestSourceKitIndexHint(t *testing.T) {
	for _, c := range []struct {
		version, want string
	}{
		{"Swift version 6.4.1 (swift-6.4.1-RELEASE)", ""},
		{"Apple Swift version 6.1 (swiftlang-6.1.0.110.21 clang-1700.0.13.3)", ""},
		{"Apple Swift version 6.0.3 (swiftlang-6.0.3.1.10 clang-1600.0.30.1)", "backgroundIndexing"},
		{"Swift version 5.10.1 (swift-5.10.1-RELEASE)", "swift build"},
		{"", "could not read"},
	} {
		got := SourceKitIndexHint(c.version)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("SourceKitIndexHint(%q) = %q, want it to contain %q", c.version, got, c.want)
		}
	}
	if h := SourceKitIndexHint("Swift version 5.10"); strings.Contains(h, "backgroundIndexing") {
		t.Errorf("5.10 has no backgroundIndexing setting: %q", h)
	}
}
