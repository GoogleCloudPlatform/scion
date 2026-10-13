package sub

// policy allows or denies actions.
type Policy struct {
	name  string
	allow bool
}

var DefaultName = "default"

const MaxRules = 3

func NewPolicy(name string, allow bool) *Policy { return &Policy{name: name, allow: allow} }

func (p *Policy) Decide(action string) bool { return p.allow && action != "" }

// Evaluate describes a decision.
func Evaluate(p *Policy, action string) string {
	if p.Decide(action) {
		return p.name + ": allow " + action
	}
	return p.name + ": deny " + action
}

// Deny returns a policy that denies everything.
func Deny(name string) *Policy { return NewPolicy(name, false) }
