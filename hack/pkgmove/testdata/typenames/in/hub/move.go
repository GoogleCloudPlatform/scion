package hub

// Token is printed with %T and registered with gob by staying code.
type Token struct{ V string }

type secret struct{ v int }

// NewSecret returns an unexported type.
func NewSecret() any { return secret{v: 1} }
