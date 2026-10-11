package hub

import "example.com/fx/hub/sub"

var registered = registerBuiltins()

func registerBuiltins() bool { return register("builtin") }

func BuiltinCount() int { return sub.BuiltinCount }
