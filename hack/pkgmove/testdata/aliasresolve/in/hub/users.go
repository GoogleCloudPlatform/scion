package hub

import "io"

// report uses every moved kind, so the move aliases them all.
type report struct {
	errBox
	c code
	b box[int]
}

func useAll(w io.Writer) error {
	r := report{errBox: errBox{Msg: "m"}, c: codeNotFound, b: box[int]{V: first([]int{1})}}
	_ = r.Text()
	_ = guard(func() {})
	_ = callerAt()
	_ = WriteJSON(w, "x")
	return writeError(w, r.c, "y")
}
