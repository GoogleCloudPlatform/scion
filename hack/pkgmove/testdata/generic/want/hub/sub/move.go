package sub

type box[T any] struct{ v T }

func (b box[T]) Run() string { return "box" }

// NewBox returns a box as an empty interface.
func NewBox() any { return box[int]{v: 1} }
