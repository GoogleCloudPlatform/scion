//go:build no_sqlite

package hub

func Fallback() int { return otherLimit() }
