// Package hub is the fixture source package.
package hub

import "fmt"

// Server stays behind and uses moved symbols.
type Server struct {
	w *widget
}

// Count calls an unexported method of a moved type.
func (s *Server) Count() int {
	if s.w == nil {
		s.w = newWidget("x")
	}
	return s.w.count() + maxItems
}

// Lookup reads a moved package-level var.
func Lookup(name string) int {
	registry[name]++
	return registry[name]
}

// Describe uses a moved generic function and a moved generic type.
func Describe() string {
	p := Pair[string, int]{K: "a", V: 1}
	xs := Map([]int{1, 2}, func(i int) int { return i * 2 })
	n, err := helper(1, "a", "b")
	return fmt.Sprint(p, xs, n, err, Exported().Name)
}

var startCount = maxItems + len(registry)

// Safe runs f and converts a panic into an error through a moved helper.
func Safe(f func()) (err error) {
	defer guard(&err)
	f()
	return nil
}
