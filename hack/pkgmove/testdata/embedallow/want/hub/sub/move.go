package sub

type Inner struct {
	Value int
}

// Outer embeds inner.
type Outer struct {
	Inner
	Name string
}
