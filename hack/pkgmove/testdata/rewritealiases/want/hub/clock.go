package hub

import "example.com/fx/other"

// Clock is a hand-written alias of another package.
type Clock = other.Clock

type fixed int64

func (f fixed) Now() int64 { return int64(f) }

func now(c Clock) int64 { return c.Now() }
