package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXcodeHints(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A CocoaPods app: the workspace is what builds, and it shares one
	// scheme.
	mk("pods/App.xcodeproj/xcshareddata/xcschemes/App.xcscheme")
	mk("pods/App.xcworkspace/contents.xcworkspacedata")
	// A plain project with no shared scheme.
	mk("plain/Tool.xcodeproj/project.pbxproj")
	// Already configured: no hint.
	mk("done/Done.xcodeproj/project.pbxproj")
	mk("done/buildServer.json")

	xcode := map[string][]string{
		filepath.Join(root, "pods"):  {"App.xcodeproj", "App.xcworkspace"},
		filepath.Join(root, "plain"): {"Tool.xcodeproj"},
		filepath.Join(root, "done"):  {"Done.xcodeproj"},
	}
	rel := func(d string) string { r, _ := filepath.Rel(root, d); return r }
	got := XcodeHints(xcode, rel)
	if len(got) != 2 {
		t.Fatalf("hints = %q, want 2 (done/ has a buildServer.json)", got)
	}
	if want := "xcode-build-server config -project Tool.xcodeproj -scheme <scheme>"; !strings.HasPrefix(got[0], "swift: plain ") || !strings.HasSuffix(got[0], want) {
		t.Errorf("plain hint = %q, want it to end %q", got[0], want)
	}
	if want := "xcode-build-server config -workspace App.xcworkspace -scheme App"; !strings.HasSuffix(got[1], want) {
		t.Errorf("pods hint = %q, want it to end %q", got[1], want)
	}
	for _, h := range got {
		if !strings.Contains(h, "brew install xcode-build-server") {
			t.Errorf("hint does not say how to install the build server: %q", h)
		}
	}
}
