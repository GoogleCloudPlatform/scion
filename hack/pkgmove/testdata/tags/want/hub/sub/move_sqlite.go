//go:build !no_sqlite

package sub

func SqliteLimit() int { return 10 }
