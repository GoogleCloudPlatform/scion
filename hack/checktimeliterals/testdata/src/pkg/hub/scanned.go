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

// A copy holds whatever its source may hold anywhere in the function.
func copied(row *sql.Row, b []byte, ts []time.Time) []string {
	var t time.Time
	_ = row.Scan(&t)
	u := t
	var j time.Time
	_ = json.Unmarshal(b, &j)
	v := j.Add(time.Second)
	var out []string
	out = append(out, u.Format(time.RFC3339)) // want format-utc
	out = append(out, v.Format(time.RFC3339)) // want format-utc
	l := time.Now().UTC()
	for _, x := range ts {
		w := l
		out = append(out, w.Format(time.RFC3339)) // want format-utc
		l = x
	}
	// Copies of values that stay UTC: clean.
	var z time.Time
	zc := z
	n := time.Now().UTC()
	nc := n.Add(time.Minute)
	out = append(out, zc.Format(time.RFC3339), nc.Format(time.RFC3339))
	return out
}
