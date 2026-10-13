package hub

import (
	"encoding/gob"
	"fmt"
)

func init() {
	gob.Register(Token{})
	gob.Register([]Token{})
	gob.RegisterName("fx.secret", secret{})
}

// Describe prints the dynamic type name.
func Describe(v any) string { return fmt.Sprintf("%T", v) }
