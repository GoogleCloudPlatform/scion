package agent

import "time"

// Not in the scanned path list.
func other(t time.Time) string { return t.Format("2006-01-02 15:04") }
