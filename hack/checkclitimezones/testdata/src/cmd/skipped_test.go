package cmd

import "time"

// Test files are not scanned.
func skipped(t time.Time) string { return t.Format("2006-01-02 15:04") }
