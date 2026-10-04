package agent

import "time"

func warn(t time.Time) string {
	return "soft-deleted at " + t.Format("2006-01-02 15:04") // want zoneless-layout
}
