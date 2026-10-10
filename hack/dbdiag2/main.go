package main

import (
	"context"
	"fmt"
	"github.com/jackc/pgx/v5"
	"os"
	"time"
)

func main() {
	ctx := context.Background()
	c, _ := pgx.Connect(ctx, os.Getenv("PG_DSN"))
	defer func() { _ = c.Close(ctx) }()
	for i := 0; i < 14; i++ {
		rows, _ := c.Query(ctx, `SELECT client_addr::text, state, count(*) FROM pg_stat_activity WHERE datname='scion_test' AND client_addr IS NOT NULL GROUP BY 1,2 ORDER BY 1,2`)
		m := map[string]int{}
		for rows.Next() {
			var a, s string
			var n int
			_ = rows.Scan(&a, &s, &n)
			m[a+"/"+s] = n
		}
		rows.Close()
		var locks, waiting int
		_ = c.QueryRow(ctx, "SELECT count(*) FROM pg_locks WHERE locktype='advisory'").Scan(&locks)
		_ = c.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Client' AND datname='scion_test'").Scan(&waiting)
		fmt.Printf("t+%2ds locks=%d %v\n", i*5, locks, m)
		time.Sleep(5 * time.Second)
	}
}
