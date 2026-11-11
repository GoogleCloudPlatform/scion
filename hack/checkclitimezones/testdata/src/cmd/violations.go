package cmd

import (
	"fmt"
	"time"
)

const tableLayout = "2006-01-02 15:04" // want zoneless-layout

var clockLayout = "15:04:05" // want zoneless-layout

func violations(t time.Time) {
	fmt.Println(t.Format("2006-01-02"))           // want zoneless-layout
	fmt.Println(t.Format("Jan 2 3:04PM"))         // want zoneless-layout
	fmt.Println(t.Format("03:04 PM"))             // want zoneless-layout
	fmt.Println(t.Format(time.Kitchen))           // want zoneless-const
	fmt.Println(t.Format(time.DateTime))          // want zoneless-const
	_ = t.AppendFormat(nil, time.StampMilli)      // want zoneless-const
	fmt.Println(t.Format(tableLayout + "Z"))      // concatenation: checked at the const
	fmt.Println(t.Format(`2006-01-02T15:04:05Z`)) // want zoneless-layout
	_ = time.ANSIC                                // want zoneless-const
	_ = clockLayout
}
