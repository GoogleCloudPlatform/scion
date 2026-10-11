// Package other is aliased by hand in hub.
package other

// Clock reads the time.
type Clock interface{ Now() int64 }
