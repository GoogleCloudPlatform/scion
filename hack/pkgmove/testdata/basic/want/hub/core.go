// Package hub is the fixture source package.
package hub

import (
	"fmt"
	"reflect"

	"example.com/fx/hub/sub"
)

// Server stays behind and uses moved symbols.
type Server struct {
	w *widget
}

// Count calls an unexported method of a moved type.
func (s *Server) Count() int {
	if s.w == nil {
		s.w = newWidget("x")
	}
	return s.w.Count() + maxItems
}

// Lookup reads a moved package-level var.
func Lookup(name string) int {
	sub.Registry[name]++
	return sub.Registry[name]
}

// Describe uses a moved generic function and a moved generic type.
func Describe() string {
	p := Pair[string, int]{K: "a", V: 1}
	xs := Map([]int{1, 2}, func(i int) int { return i * 2 })
	n, err := helper(1, "a", "b")
	return fmt.Sprint(p, xs, n, err, Exported().Name)
}

var startCount = maxItems + len(sub.Registry)

// Safe runs f and converts a panic into an error through a moved helper.
func Safe(f func()) (err error) {
	defer guard(&err)
	f()
	return nil
}

// summaryTemplate calls a method by name at run time.
const summaryTemplate = "{{.Count}} items"

// Origin reports the caller file through a moved helper.
func Origin() string { return whereVia() }

// hasCount looks the method up by name.
func hasCount(v any) bool {
	_, ok := reflect.TypeOf(v).MethodByName("Count")
	return ok
}
