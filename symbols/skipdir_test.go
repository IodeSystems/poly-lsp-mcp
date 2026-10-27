package symbols

import "testing"

func TestIsSkipDir(t *testing.T) {
	for name, want := range map[string]bool{
		"node_modules": true, "Pods": true, "Carthage": true, ".build": true,
		".swiftpm": true, "DerivedData": true, "xcuserdata": true,
		"Assets.xcassets": true, "Media.xcassets": true,
		// The suffix alone is not a name, and the bundles that mark a
		// Swift project root are not skipped by the walk itself.
		".xcassets": false, "Demo.xcodeproj": false, "Sources": false, "pods": false,
	} {
		if got := IsSkipDir(name); got != want {
			t.Errorf("IsSkipDir(%q) = %v, want %v", name, got, want)
		}
	}
}
