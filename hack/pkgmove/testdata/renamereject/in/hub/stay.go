package hub

type labeler interface{ label() string }

// Use reaches the moved code.
func Use() (int, string) {
	var l labeler = item{}
	return helper(), l.label()
}
