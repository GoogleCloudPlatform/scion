package hub

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
)

const dayLayout = "2006-01-02"

const notALayout = "hello"

type event struct {
	CreatedAt time.Time
	Name      string
}

func formatClean(t time.Time, e event, ts *timestamppb.Timestamp) []string {
	now := time.Now().UTC()
	later := now.Add(time.Hour)
	day := e.CreatedAt.UTC().Truncate(24 * time.Hour)
	var buf []byte
	buf = e.CreatedAt.UTC().AppendFormat(buf, time.RFC3339)
	return []string{
		t.UTC().Format(time.RFC3339Nano),
		(t.UTC()).Format(time.RFC3339),
		now.Format(time.RFC3339),
		later.Format(dayLayout),
		day.Format(dayLayout),
		ts.AsTime().Format(dayLayout),
		time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		e.Name,
		fmt.Sprint(len(buf)),
		// Not time layouts: never flagged.
		t.Format(notALayout),
		fmt.Sprintf("%s", "x"),
	}
}

func formatViolations(t time.Time, e event, layout string) []string {
	now := time.Now()
	mixed := time.Now().UTC()
	if len(layout) > 3 {
		mixed = time.Now()
	}
	var buf []byte
	buf = e.CreatedAt.AppendFormat(buf, time.RFC3339) // want format-utc
	return []string{
		t.Format(time.RFC3339),                     // want format-utc
		e.CreatedAt.Format("2006-01-02T15:04:05Z"), // want format-utc
		now.Format(time.RFC3339Nano),               // want format-utc
		mixed.Format(time.Kitchen),                 // want format-utc
		t.Local().Format(dayLayout),                // want format-utc
		t.Add(time.Hour).Format(time.DateTime),     // want format-utc
		t.In(time.UTC).Format(time.DateOnly),       // want format-utc
		t. // want format-utc
			Format(
				time.RFC3339),
		string(buf),
		// A layout held in a parameter is a documented blind spot.
		t.Format(layout),
	}
}

var packageLevel = time.Now().Format(time.RFC3339) // want format-utc
