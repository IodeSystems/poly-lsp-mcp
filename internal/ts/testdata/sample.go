package p

import "fmt"

type S struct{ X int }

func (s *S) A(x int) string {
	y := x + s.X
	return fmt.Sprint(y, `raw`)
}
