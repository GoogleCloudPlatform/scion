package hub

type rec struct {
	a    int8
	name string
}

var counter int

func Bump() int { counter++; return counter }

func NewRec() rec { return rec{name: "x"} }
