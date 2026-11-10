package mcp

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestBuildThreadIsNiced(t *testing.T) {
	var tid int
	niceHook = func(id int) { tid = id }
	defer func() { niceHook = nil }()
	s := New(nil, "", nil, nil)
	var got int
	err := s.runBuild(func() error {
		b, err := os.ReadFile(fmt.Sprintf("/proc/self/task/%d/stat", tid))
		if err != nil {
			return err
		}
		// Fields after the ")" closing comm: state is [0], nice is [16].
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
		got, err = strconv.Atoi(f[16])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != buildNice {
		t.Fatalf("nice during build = %d, want %d", got, buildNice)
	}
}
