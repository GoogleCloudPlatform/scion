package sub

import "fmt"

// Copied from hub's test helpers, so test waves can reuse them.
func mkPolicy(name string) *Policy { return NewPolicy(name, true) }

const testAction = "read"

func describe(p *Policy) string { return fmt.Sprint(p.Decide(testAction)) }
