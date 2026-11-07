package p

type Map[K comparable, V any] struct{}

func (m *Map[K, V]) f() {}

const (
	a = iota
	b
)
