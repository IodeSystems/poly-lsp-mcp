package graphq

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Bank is the shape of one entry in bank.json.
type Bank struct {
	ID       string            `json:"id"`
	Question string            `json:"question"`
	Expected []string          `json:"expected"`
	Category string            `json:"category"`
	Canonical map[string]string `json:"canonical"`
}

var funcNameRe = regexp.MustCompile(`^func\s+(\([^)]+\)\s+)?(\w+)`)

// topLevelFuncNames reads a Go source file and returns the names of all
// top-level function declarations (lines starting with "func ").
func topLevelFuncNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	var names []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "func ") {
			continue
		}
		m := funcNameRe.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("%s: cannot parse func line: %s", path, line)
		}
		names = append(names, m[2])
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return names
}

// TestFileScopedQuestionsMatchSource verifies that every "functions_in_file"
// question in bank.json lists exactly the functions declared in the named
// file. This catches undercounts (missing a function) and overcounts
// (listing a function that isn't there).
func TestFileScopedQuestionsMatchSource(t *testing.T) {
	// Resolve the mcp/ directory relative to this test's package.
	// graphq/ is at the repo root, so mcp/ is ../mcp/.
	mcpDir := filepath.Join("..", "mcp")
	if _, err := os.Stat(mcpDir); err != nil {
		t.Skipf("mcp/ not found relative to graphq/: %v", err)
	}

	data, err := os.ReadFile("bank.json")
	if err != nil {
		t.Fatalf("read bank.json: %v", err)
	}
	var bank []Bank
	if err := json.Unmarshal(data, &bank); err != nil {
		t.Fatalf("unmarshal bank.json: %v", err)
	}

	fileRe := regexp.MustCompile(`mcp/(\w+\.go)` )
	for _, q := range bank {
		if q.Category != "functions_in_file" {
			continue
		}
		m := fileRe.FindStringSubmatch(q.Question)
		if m == nil {
			t.Errorf("%s: cannot extract file path from question: %q", q.ID, q.Question)
			continue
		}
		relPath := m[0] // e.g. "mcp/query_text.go"
		srcPath := filepath.Join("..", relPath)

		actualNames := topLevelFuncNames(t, srcPath)
		expectedNames := make([]string, len(q.Expected))
		for i, addr := range q.Expected {
			// addr is like "mcp/query_text.go#QueryText"
			parts := strings.SplitN(addr, "#", 2)
			if len(parts) != 2 {
				t.Errorf("%s: expected entry %q has no # separator", q.ID, addr)
				continue
			}
			expectedNames[i] = parts[1]
		}

		sort.Strings(actualNames)
		sort.Strings(expectedNames)

		if len(actualNames) != len(expectedNames) {
			t.Errorf("%s (%s): expected %d functions, bank has %d.\n  actual:   %v\n  expected: %v",
				q.ID, relPath, len(actualNames), len(expectedNames), actualNames, expectedNames)
			continue
		}
		for i := range actualNames {
			if actualNames[i] != expectedNames[i] {
				t.Errorf("%s (%s): mismatch at index %d: source has %q, bank has %q",
					q.ID, relPath, i, actualNames[i], expectedNames[i])
			}
		}
	}
}
