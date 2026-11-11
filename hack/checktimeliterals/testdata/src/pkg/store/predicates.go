package store

import (
	"time"

	"entgo.io/ent/dialect/sql"
)

func predicates(t time.Time) []*sql.Predicate {
	cursor := t.UTC().Format(time.RFC3339Nano)
	return []*sql.Predicate{
		sql.LT("created", t.UTC()),
		sql.GT("created", cursor), // want ent-bind-formatted
		sql.EQ("name", "x"),
	}
}
