// Command dbdiag prints CloudSQL connection usage for diagnosing pool
// saturation. Not part of the product.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"
)

func main() {
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, os.Getenv("PG_DSN"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var maxc, used int
	conn.QueryRow(ctx, "SHOW max_connections").Scan(&maxc)
	conn.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE datname='scion_test'").Scan(&used)
	fmt.Printf("max_connections=%d  total_on_scion_test=%d\n", maxc, used)

	rows, _ := conn.Query(ctx, `SELECT COALESCE(application_name,'(none)'), state, count(*)
		FROM pg_stat_activity WHERE datname='scion_test'
		GROUP BY 1,2 ORDER BY 3 DESC`)
	defer rows.Close()
	fmt.Printf("%-32s %-20s %s\n", "application_name", "state", "count")
	for rows.Next() {
		var app, state string
		var n int
		rows.Scan(&app, &state, &n)
		fmt.Printf("%-32s %-20s %d\n", app, state, n)
	}
	// Advisory locks currently held.
	var locks int
	conn.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory'").Scan(&locks)
	fmt.Printf("advisory_locks_held=%d\n", locks)
}
