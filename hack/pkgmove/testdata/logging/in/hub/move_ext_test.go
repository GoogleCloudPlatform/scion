package hub_test

import (
	stdlog "log"
	"testing"

	"example.com/fx/hub"
)

func TestWire(t *testing.T) {
	stdlog.Print("wire")
	_ = stdlog.Default() // builds a value only: not reported
	hub.Wire()
}
