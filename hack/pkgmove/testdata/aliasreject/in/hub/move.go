package hub

import "io"

// wrapped embeds an alias whose target has another name.
type wrapped struct{ errBox }

func Move(w io.Writer) error {
	_ = wrapped{}
	_ = callerAt()
	_ = hook(w, "h")
	_ = writeJSON(w, "j")
	_ = WriteJSON(w, "k")
	return notFound(w, "m")
}
