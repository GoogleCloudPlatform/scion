package store

import "time"

func inTest(t time.Time) string { return t.Format(time.RFC3339) }
