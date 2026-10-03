package entadapter

import (
	"time"

	entsql "entgo.io/ent/dialect/sql"
)

// The codebase imports the ent dialect package as entsql; predicates are
// matched on the import path, not on the identifier sql.
func aliased(now time.Time, s *entsql.Selector) []*entsql.Predicate {
	formatted := now.UTC().Format(time.RFC3339Nano)
	s.Where(entsql.P(func(b *entsql.Builder) {
		b.WriteString("next_run_at <= ").Arg(now.UTC())
		b.WriteString("next_run_at <= ").Arg(formatted) // want ent-bind-formatted
		b.Args(now.UTC().Format(time.RFC3339Nano))      // want ent-bind-formatted
	}))
	return []*entsql.Predicate{
		entsql.LTE("next_run_at", now.UTC()),
		entsql.LTE("next_run_at", now.UTC().Format(time.RFC3339Nano)), // want ent-bind-formatted
		entsql.FieldLT("created", formatted),                          // want ent-bind-formatted
		entsql.ExprP("created < ? AND id <> ?", formatted, "x"),       // want ent-bind-formatted
		entsql.In("id", "a", "b"),
	}
}

// A Builder passed to a plain function is checked too.
func builderParam(b *entsql.Builder, now time.Time) {
	b.Arg(now.UTC())
	b.Arg(now.UTC().Format(time.RFC3339Nano)) // want ent-bind-formatted
}
