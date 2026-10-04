package sub

import (
	"fmt"
	stdtime "time"
)

func alias(t stdtime.Time, s string) {
	fmt.Println(t.Format(stdtime.TimeOnly))   // want zoneless-const
	_, _ = stdtime.Parse(stdtime.DateOnly, s) // exempt: parse layout
}
