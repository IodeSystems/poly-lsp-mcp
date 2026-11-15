package skipdirs

import "testing"

func TestIs(t *testing.T) {
	for name, want := range map[string]bool{
		".claude": false, "node_modules": true, "Assets.xcassets": true,
		"src": false, ".claudex": false,
	} {
		if got := Is(name); got != want {
			t.Errorf("Is(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsWatchSkipAddsClaude(t *testing.T) {
	if !IsWatchSkip(".claude") || !IsWatchSkip("node_modules") || IsWatchSkip("src") {
		t.Error("IsWatchSkip wrong")
	}
}
