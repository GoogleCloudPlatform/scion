package sub

// Token is printed with %T and registered with gob by staying code.
type Token struct{ V string }

type Secret struct{ v int }

// NewSecret returns an unexported type.
func NewSecret() any { return Secret{v: 1} }
