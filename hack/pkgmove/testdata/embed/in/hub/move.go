package hub

type inner struct {
	Value int
}

// Outer embeds inner.
type Outer struct {
	inner
	Name string
}
