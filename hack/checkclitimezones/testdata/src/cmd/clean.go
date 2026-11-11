package cmd

import (
	"fmt"
	"time"
)

const zonedLayout = "2006-01-02 15:04 MST"

func clean(t time.Time, s string) {
	fmt.Println(t.Format(time.RFC3339))
	fmt.Println(t.Format(time.RFC1123Z))
	fmt.Println(t.Format(time.UnixDate))
	fmt.Println(t.Format(zonedLayout))
	fmt.Println(t.Format("2006-01-02 15:04:05 -07:00"))
	fmt.Println(t.Format("15:04 -0700"))
	fmt.Println(t.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Println(t.Format("15:04:05.000 Z0700"))
	fmt.Println(t.Format("2006-01-02 -07"))

	// Parse layouts describe input and are exempt.
	_, _ = time.Parse("2006-01-02", s)
	_, _ = time.Parse(time.DateTime, s)
	_, _ = time.ParseInLocation("2006-01-02 15:04", s, time.Local)

	// Strings that are not layouts.
	fmt.Println("version 1.2.3, port 8080, 15 minutes")
	fmt.Println("status: running")
}
