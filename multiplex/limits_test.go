package multiplex

import (
	"reflect"
	"testing"
)

func TestClangdJobs(t *testing.T) {
	for ncpu, want := range map[int]int{0: 1, 1: 1, 2: 1, 3: 2, 8: 4, 15: 8, 16: 8, 64: 8} {
		if got := clangdJobs(ncpu); got != want {
			t.Errorf("clangdJobs(%d) = %d, want %d", ncpu, got, want)
		}
	}
}

func TestWithLimits(t *testing.T) {
	cases := []struct {
		name string
		path string
		args []string
		want []string
	}{
		{"clangd no args", "/usr/bin/clangd", nil, []string{"-j=4"}},
		{"clangd with args", "/usr/bin/clangd", []string{"--background-index"}, []string{"--background-index", "-j=4"}},
		{"versioned", "/usr/bin/clangd-17", nil, []string{"-j=4"}},
		{"exe", `clangd.exe`, nil, []string{"-j=4"}},
		{"user -j=N", "clangd", []string{"-j=2"}, []string{"-j=2"}},
		{"user -jN", "clangd", []string{"-j2"}, []string{"-j2"}},
		{"user -j N", "clangd", []string{"-j", "2"}, []string{"-j", "2"}},
		{"user --j=N", "clangd", []string{"--j=2"}, []string{"--j=2"}},
		{"other command", "/usr/bin/gopls", nil, nil},
		{"clangd-format", "/usr/bin/clangd-format", nil, nil},
		{"not clangd dir", "/opt/clangd/bin/pylsp", []string{"-v"}, []string{"-v"}},
	}
	for _, c := range cases {
		in := append([]string(nil), c.args...)
		got := withLimits(c.path, c.args, 8)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
		if !reflect.DeepEqual(c.args, in) {
			t.Errorf("%s: input modified", c.name)
		}
	}
}
