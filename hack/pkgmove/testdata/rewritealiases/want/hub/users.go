package hub

import (
	"io"

	"example.com/fx/apierr"
)

// report uses every moved kind, so the move aliases them all.
type report struct {
	errBox
	c apierr.Code
	b apierr.Box[int]
}

func useAll(w io.Writer) error {
	r := report{errBox: apierr.ErrBox{Msg: "m"}, c: apierr.CodeNotFound, b: apierr.Box[int]{V: apierr.First([]int{1})}}
	_ = r.Text()
	_ = apierr.Guard(func() {})
	_ = apierr.CallerAt()
	_ = apierr.WriteJSON(w, "x")
	return apierr.WriteError(w, r.c, "y")
}
