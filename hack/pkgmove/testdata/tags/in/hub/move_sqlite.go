//go:build !no_sqlite

package hub

func sqliteLimit() int { return 10 }
