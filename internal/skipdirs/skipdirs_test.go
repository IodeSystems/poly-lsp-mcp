package skipdirs

import "testing"

func TestIs(t *testing.T) {
	for name, want := range map[string]bool{
		// The five Python tool dirs added for broad queries (a virtualenv is
		// vendored third-party, often 15x the first-party lines).
		".venv": true, "venv": true, ".tox": true,
		"site-packages": true, ".eggs": true,
		// Existing entries, kept so a reorder does not drop one silently.
		"node_modules": true, "vendor": true, "Pods": true,
		// Is is an exact map lookup plus the .xcassets suffix: a name that
		// merely resembles one is not skipped. vendor is itself in the list,
		// so it is not a usable negative here.
		"vendors": false, "mysite-packages": false, ".venvx": false,
		"xcassets": false, "Sources": false,
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
