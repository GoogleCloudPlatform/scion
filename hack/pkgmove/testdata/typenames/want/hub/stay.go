package hub

import (
	"encoding/gob"
	"fmt"
)

func init() {
	gob.Register(Token{})
}

// Describe prints the dynamic type name.
func Describe(v any) string { return fmt.Sprintf("%T", v) }
