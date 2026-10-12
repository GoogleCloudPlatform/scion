package sub

import (
	"fmt"
	"testing"
)

// mkPolicy differs from hub's (it denies).
func mkPolicy(name string) *Policy { return NewPolicy(name, false) }

const testAction = "read"

func describe(p *Policy) string { return fmt.Sprint(p.Decide(testAction)) }

func TestDecide(t *testing.T) {}
