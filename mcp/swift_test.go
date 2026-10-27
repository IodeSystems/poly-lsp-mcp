package mcp

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// swiftWorkspace copies testdata/fixtures/swift (a SwiftPM package and an
// Xcode app) into a temp dir and adds what an iOS checkout carries beside
// its code: vendored and generated trees the index must leave out.
func swiftWorkspace(t *testing.T) (*mcpSession, string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join("..", "testdata", "fixtures", "swift")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("iOSApp/Pods/Alamofire/Source/Session.swift", "open class PodsSession {}\n")
	write("iOSApp/DerivedData/Demo/Build/Intermediates.noindex/Gen.swift", "class DerivedGen {}\n")
	write(".build/checkouts/swift-log/Sources/Logging/Logger.swift", "public struct BuildLogger {}\n")
	write("iOSApp/Carthage/Checkouts/Lib/Lib.swift", "class CarthageLib {}\n")
	write("iOSApp/Demo/Assets.xcassets/AppIcon.appiconset/Contents.json", `{"images": [{"idiom": "universal", "filename": "icon.png", "scale": "2x"}]}`+"\n")
	write("iOSApp/Demo.xcodeproj/xcuserdata/me.xcuserdatad/Junk.swift", "class XcuserJunk {}\n")

	s := startSessionFull(t, dir, nil, nil)
	s.request("initialize", map[string]any{})
	s.notify("notifications/initialized", map[string]any{})
	return s, dir
}

func TestSwiftNodeAddressesAndReads(t *testing.T) {
	s, _ := swiftWorkspace(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `#'Sources/App/Greeter.swift' method`, "limit": 100})
	for _, want := range []string{
		"Sources/App/Greeter.swift#Greeter.greet",
		"Sources/App/Greeter.swift#Greeter.wave", // from the merged extension
		"Sources/App/Greeter.swift#Greeting.greet",
		"Sources/App/Greeter.swift#String.shouted",
	} {
		if !hasNode(q, want) {
			t.Errorf("method query missing %s; got %v", want, nodes(q))
		}
	}

	// A merged extension member is a CHILD of its type in the tree.
	kids := query(t, s, map[string]any{"selector": `#'Sources/App/Greeter.swift#Greeter' > method`, "limit": 100})
	if !hasNode(kids, "Sources/App/Greeter.swift#Greeter.wave") {
		t.Errorf("Greeter > method should include the extension's wave; got %v", nodes(kids))
	}

	// Attributes compose like annotations elsewhere.
	ib := query(t, s, map[string]any{"selector": `method:any(annotation#IBAction)`, "limit": 100})
	if !hasNode(ib, "iOSApp/Demo/ViewController.swift#ViewController.tap") || len(ib.Matches) != 1 {
		t.Errorf("method:any(annotation#IBAction) = %v, want only ViewController.tap", nodes(ib))
	}

	// node_read returns the declaration with its doc comment and attribute.
	m, isErr := readNode(t, s, map[string]any{"node": "Sources/App/Greeter.swift#Greeter.greet"})
	if isErr {
		t.Fatalf("node_read errored: %+v", m)
	}
	text, _ := m["text"].(string)
	if !strings.HasPrefix(text, "/* Greets someone. */\n    @objc func greet(") || !strings.HasSuffix(text, "return who\n    }") {
		t.Errorf("node_read Greeter.greet = %q", text)
	}

	sig := query(t, s, map[string]any{"selector": `#'Sources/App/Greeter.swift#Greeter.greet'::signature`})
	if len(sig.Matches) != 1 || !strings.Contains(sig.Matches[0].Text, "func greet(_ who: String, times: Int = 1) -> String") ||
		strings.Contains(sig.Matches[0].Text, "count += times") {
		t.Errorf("::signature = %+v", sig.Matches)
	}
}

// References are lexical (sourcekit-lsp is not on this machine's PATH in
// CI's Linux leg), and rename runs over the index across files.
func TestSwiftReferencesAndRename(t *testing.T) {
	s, dir := swiftWorkspace(t)
	defer s.close()

	in := query(t, s, map[string]any{"selector": `func#makeGreeter::in`, "limit": 50})
	found := false
	for _, n := range nodes(in) {
		if strings.HasPrefix(n, "iOSApp/Demo/ViewController.swift") {
			found = true
		}
	}
	if !found {
		t.Errorf("makeGreeter::in should reach ViewController.swift; got %v", nodes(in))
	}

	renameReply(t, s.callTool("node_edit", map[string]any{"node": "Sources/App/Models.swift#makeGreeter", "rename": "buildGreeter"}))
	models, _ := os.ReadFile(filepath.Join(dir, "Sources/App/Models.swift"))
	vc, _ := os.ReadFile(filepath.Join(dir, "iOSApp/Demo/ViewController.swift"))
	if !strings.Contains(string(models), "func buildGreeter(named name: String)") {
		t.Errorf("declaration not renamed:\n%s", models)
	}
	if !strings.Contains(string(vc), "buildGreeter(named: \"iOS\")") || strings.Contains(string(vc), "makeGreeter") {
		t.Errorf("call site not renamed:\n%s", vc)
	}
}

// Vendored and generated iOS trees are left out of a broad walk, and a
// selector that names a path inside one still reaches it.
func TestSwiftSkipsVendoredAndGeneratedTrees(t *testing.T) {
	s, _ := swiftWorkspace(t)
	defer s.close()

	q := query(t, s, map[string]any{"selector": `class`, "limit": 200})
	for _, n := range nodes(q) {
		for _, leak := range []string{"PodsSession", "DerivedGen", "BuildLogger", "CarthageLib", "XcuserJunk"} {
			if strings.Contains(n, leak) {
				t.Errorf("%s leaked into a broad query: %s", leak, n)
			}
		}
	}
	if !hasNode(q, "iOSApp/Demo/ViewController.swift#ViewController") {
		t.Errorf("the app's own class is missing; got %v", nodes(q))
	}
	files := query(t, s, map[string]any{"selector": `file`, "limit": 200})
	for _, n := range nodes(files) {
		if strings.Contains(n, "Contents.json") || strings.Contains(n, ".xcassets") {
			t.Errorf("asset catalog internals reached the tree: %s", n)
		}
	}

	named := query(t, s, map[string]any{"selector": `#'iOSApp/Pods/Alamofire/Source/Session.swift' class`})
	if !hasNode(named, "iOSApp/Pods/Alamofire/Source/Session.swift#PodsSession") {
		t.Errorf("a path named inside Pods/ should be reachable; got %v", nodes(named))
	}
}
