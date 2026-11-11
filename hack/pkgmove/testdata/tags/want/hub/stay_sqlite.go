//go:build !no_sqlite

package hub

func UseSQLite() int { return sqliteLimit() }
