package hub

import "time"

// Aliases of time.<Layout> are layouts too.
const wireLayout = time.RFC3339Nano

var varLayout = time.RFC3339

const kitchen = time.Kitchen

func aliases(t time.Time) []string {
	layout := time.RFC3339
	const inner = time.DateOnly
	copied := wireLayout
	return []string{
		t.Format(wireLayout),       // want format-utc
		t.Format(varLayout),        // want format-utc
		t.Format(layout),           // want format-utc
		t.Format(inner),            // want format-utc
		t.Format(copied),           // want format-utc
		t.Format(kitchen),          // want format-utc
		t.UTC().Format(wireLayout), // converted first: clean
		t.UTC().Format(layout),
		t.UTC().Format(inner),
	}
}

// A local that is not a layout does not make Format a time format.
func notLayout(d decimal) string {
	prec := "0.00"
	return d.Format(prec)
}

type decimal struct{}

func (decimal) Format(string) string { return "" }
