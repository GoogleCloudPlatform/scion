package sub_test

import (
	stdlog "log"
	"testing"

	"example.com/fx/hub/sub"
)

func TestWire(t *testing.T) {
	stdlog.Print("wire")
	_ = stdlog.Default() // builds a value only: not reported
	sub.Wire()
}
