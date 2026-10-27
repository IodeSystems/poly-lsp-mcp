#!/bin/sh
# Regenerates the grammar blobs and their generated Go files from the
# parser.c files of the smacker/go-tree-sitter version pinned in go.mod, so the
# pure-Go runtime parses with exactly the grammars the cgo runtime links.
# Run via `go generate ./internal/ts/grammars` after moving that pin.
set -e
cd "$(dirname "$0")"
SMACKER=$(go list -m -f '{{.Dir}}' github.com/smacker/go-tree-sitter)
TS2GO="go run github.com/odvcencio/gotreesitter/cmd/ts2go@v0.56.0"
gen() { # name, directory under the smacker module
	$TS2GO -input "$SMACKER/$2/parser.c" -output "$1.go" -package grammars -name "$1"
}
gen go golang
gen java java
gen c c
gen cpp cpp
gen python python
gen tsx typescript/tsx
gen markdown markdown/tree-sitter-markdown
gen proto protobuf
gen sql sql
gen kotlin kotlin
gen groovy groovy
gen swift swift
gofmt -w .
