//go:build integration

package hub

// CanRun reports whether v has the unexported run method.
func CanRun(v any) bool {
	_, ok := v.(interface{ run() string })
	return ok
}
