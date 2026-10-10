package sub

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"unicode"
)

type level int

func (l level) String() string { return "lvl" }

// upper runs a func argument at init.
var upper = strings.Map(unicode.ToUpper, "abc")

// label calls a String method through fmt.
var label = fmt.Sprint(level(1))

// plain only formats basic values.
var plain = fmt.Sprintf("%d-%s", 1, "x")

// kind uses reflect beyond TypeOf.
var kind = reflect.ValueOf(1).Kind()

// out reads another package's var.
var out = os.Stderr

// Values exposes them.
func Values() []any { return []any{upper, label, plain, kind, out} }
