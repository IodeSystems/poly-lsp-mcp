// Hand-written stubs that mirror what protoc-gen-go would emit for
// greeter.proto. Each Go type is @ref-linked back to its source-of-
// truth in the proto so the poly-lsp-mcp comment scanner can stitch them
// together: a rename initiated from the proto would also touch these,
// and the diagnostic path surfaces gopls errors against this file
// when the contract drifts.
//
// This file is part of the cross-language diagnostic fixture — see
// internal/mcp diagnostic tests. Not generated; deliberately
// stripped down (no protobuf imports, no descriptor machinery) so
// gopls can index it without pulling protoc plugins into the test
// environment.
package server

// Mood is the caller's emotional state.
//
// @ref ../greeter.proto:Mood
type Mood int32

const (
	// @ref ../greeter.proto:MOOD_UNSPECIFIED
	MoodUnspecified Mood = 0
	// @ref ../greeter.proto:MoodHappy
	MoodHappy Mood = 1
	// @ref ../greeter.proto:MOOD_GRUMPY
	MoodGrumpy Mood = 2
)

// HelloRequest carries the caller's name and a mood.
//
// @ref ../greeter.proto:HelloRequest
type HelloRequest struct {
	Name string
	Mood Mood
}

// HelloResponse returns the rendered greeting.
//
// @ref ../greeter.proto:HelloResponse
type HelloResponse struct {
	Greeting string
}

// Hello renders a greeting for the supplied request.
//
// @ref ../greeter.proto:Hello
func Hello(req HelloRequest) HelloResponse {
	prefix := "hi"
	if req.Mood == MoodGrumpy {
		prefix = "ugh"
	}
	return HelloResponse{Greeting: prefix + " " + req.Name}
}
