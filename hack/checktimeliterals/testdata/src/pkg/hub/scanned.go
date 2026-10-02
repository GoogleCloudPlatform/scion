package hub

import (
	"database/sql"
	"encoding/json"
	"time"
)

// A zero time.Time is UTC, but one filled through its address holds whatever
// zone was decoded.
func scanned(row *sql.Row, b []byte) []string {
	var fromRow time.Time
	_ = row.Scan(&fromRow)
	var fromJSON time.Time
	_ = json.Unmarshal(b, &fromJSON)
	var fromText time.Time
	_ = fromText.UnmarshalText(b)
	var untouched time.Time
	return []string{
		fromRow.Format(time.RFC3339Nano),       // want format-utc
		fromJSON.Format(time.RFC3339Nano),      // want format-utc
		fromText.Format(time.RFC3339Nano),      // want format-utc
		fromRow.UTC().Format(time.RFC3339Nano), // converted first: clean
		untouched.Format(time.RFC3339Nano),     // the zero value is UTC: clean
	}
}

// Only ever assigned UTC values (the hubsync lastSyncedAt shape): clean.
func assignedUTC(raw string) string {
	var last time.Time
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		last = parsed.UTC()
	}
	return last.Format(time.RFC3339Nano)
}
