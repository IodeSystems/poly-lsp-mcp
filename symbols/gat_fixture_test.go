package symbols_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iodesystems/poly-lsp-mcp/config"
	"github.com/iodesystems/poly-lsp-mcp/symbols"
)

// TestGatGreeterFixture proves the live-gat → poly-lsp-mcp linkage across
// every format gat now carries `@ref` through (gwag commit 09df07a:
// "@ref source-of-truth marker carriage"):
//
//   - GraphQL SDL: triple-quoted `"""@ref ..."""` block-string
//     descriptions on objects / enums / fields, via the runtime
//     schema's `withRef` plumbing.
//   - OpenAPI JSON: `x-ref` extension on operations + the original
//     `@ref` text surviving inside `info.description` for the
//     service-level marker.
//   - Source `.proto`: hand-authored `// @ref` markers as written
//     (the input — what gat propagates from).
//
// We run the fixture's `cmd/dump` once per format, lay the artifacts
// next to a copy of the source proto in a temp dir, and assert that
// `symbols.Build` ends up with declared-confidence sites in EVERY
// emitted file for the expected names. That's the contract: a single
// `node_refactor(rename UserID)` against the source touches both the
// source proto AND every generated artifact.
//
// Skipped under -short because shelling out to `go run` for the
// fixture's sub-module pulls gwag's full dep tree on first invocation.
// Use `go test -run TestGatGreeterFixture ./internal/symbols` to
// exercise.
func TestGatGreeterFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping live-gat fixture test under -short")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}

	fixtureDir := fixtureRoot(t)
	protoPath := filepath.Join(fixtureDir, "greeter.proto")
	if _, err := os.Stat(protoPath); err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	// Soft-guard: replace directive points at ../../../../gwag from the
	// fixture. If that checkout isn't present, the test environment
	// can't satisfy the dep.
	if _, err := os.Stat(filepath.Join(fixtureDir, "..", "..", "..", "..", "gwag")); err != nil {
		t.Skipf("gwag checkout missing at sibling path: %v", err)
	}

	sdl := runDump(t, fixtureDir, "graphql")
	if !strings.Contains(sdl, "@ref") {
		t.Fatalf("graphql output has no @ref markers:\n%s", sdl)
	}
	openapi := runDump(t, fixtureDir, "openapi")
	// OpenAPI carries the marker in two shapes — `x-ref` on operations,
	// `@ref` text inside info.description. Either is enough to prove
	// gat re-emitted it; we assert both because they each exercise a
	// different branch of our scanner.
	if !strings.Contains(openapi, "x-ref") {
		t.Fatalf("openapi output has no x-ref extension:\n%s", openapi)
	}
	if !strings.Contains(openapi, "@ref") {
		t.Fatalf("openapi output has no @ref in description text:\n%s", openapi)
	}

	// Lay everything side by side in a temp dir. We isolate from the
	// fixture's go.mod so symbols.Build doesn't traverse unrelated
	// sources.
	workDir := t.TempDir()
	if err := copyFile(protoPath, filepath.Join(workDir, "greeter.proto")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "schema.graphql"), []byte(sdl), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "openapi.json"), []byte(openapi), 0o644); err != nil {
		t.Fatal(err)
	}

	reg, err := config.Default().Build()
	if err != nil {
		t.Fatal(err)
	}
	idx, err := symbols.Build(workDir, reg)
	if err != nil {
		t.Fatalf("symbols.Build: %v", err)
	}

	// Coverage per name + format. Each "want*" lists what gat actually
	// emits today; assertions fail if any expected artifact drops the
	// marker on the next gwag bump.
	cases := []struct {
		name         string
		wantProto    bool // hand-authored marker on the source .proto
		wantSDL      bool // gat-rendered GraphQL SDL
		wantOpenAPI  bool // gat-rendered OpenAPI JSON
	}{
		// rpc: lives in all three (source + SDL field description + OpenAPI x-ref).
		{name: "Hello", wantProto: true, wantSDL: true, wantOpenAPI: true},
		// service-level: gat puts the description text into OpenAPI
		// info.description verbatim, so the @ref text survives there.
		// Not on per-type SDL output (graphql-go schemas don't expose a
		// service-level description), so SDL is skipped intentionally.
		{name: "GreeterService", wantProto: true, wantOpenAPI: true},
		// message type: gat re-emits on the SDL type description.
		// OpenAPI components.schemas doesn't get an x-ref today —
		// that's a gat-side gap, not ours, so wantOpenAPI=false.
		{name: "HelloResponse", wantProto: true, wantSDL: true},
		// enum type: same as message type.
		{name: "Mood", wantProto: true, wantSDL: true},
		// enum value: enum-value comments survive into SDL description
		// verbatim, so our regex catches it there too.
		{name: "MoodHappy", wantProto: true, wantSDL: true},
	}

	for _, c := range cases {
		sites := idx.Lookup(c.name)
		if len(sites) == 0 {
			t.Errorf("%s: no sites in index", c.name)
			continue
		}
		seen := map[string]bool{}
		for _, s := range sites {
			if s.Confidence != symbols.ConfidenceDeclared {
				continue
			}
			seen[filepath.Base(s.File)] = true
		}
		if c.wantProto && !seen["greeter.proto"] {
			t.Errorf("%s: missing declared site in greeter.proto; sites=%+v", c.name, sites)
		}
		if c.wantSDL && !seen["schema.graphql"] {
			t.Errorf("%s: missing declared site in schema.graphql; sites=%+v", c.name, sites)
		}
		if c.wantOpenAPI && !seen["openapi.json"] {
			t.Errorf("%s: missing declared site in openapi.json; sites=%+v", c.name, sites)
		}
	}
}

// runDump invokes the fixture's cmd/dump with -format <format> and
// returns stdout. Fails the test on non-zero exit.
func runDump(t *testing.T, fixtureDir, format string) string {
	t.Helper()
	cmd := exec.Command("go", "run", "./cmd/dump", "-format", format)
	cmd.Dir = fixtureDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dump -format %s: %v\nstderr:\n%s", format, err, stderr.String())
	}
	return stdout.String()
}

func fixtureRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(here), ".."))
	return filepath.Join(repoRoot, "testdata", "fixtures", "gat-greeter")
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
