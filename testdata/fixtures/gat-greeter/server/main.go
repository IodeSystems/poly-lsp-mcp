// Main entrypoint that uses the proto-shaped types. The test edits
// this file to introduce type errors against types.go; gopls then
// publishes diagnostics that flow back through tslsmcp's MCP edit
// response (per Phase 5 diagnostic enrichment).
package server

import "fmt"

func Run() {
	req := HelloRequest{
		Name: "world",
		Mood: MoodHappy,
	}
	resp := Hello(req)
	fmt.Println(resp.Greeting)
}
