package entadapter

import (
	"database/sql"
	"time"
)

type fakeDialect struct{}

func (fakeDialect) EQ(string, any) bool { return false }

// No ent dialect import here: a local named sql is not an ent predicate, and
// database/sql's sql is not mistaken for one.
func notEnt(db *sql.DB, now time.Time) bool {
	_ = db
	sql := fakeDialect{}
	return sql.EQ("created", now.UTC().Format(time.RFC3339Nano))
}
