package hub

import "example.com/fx/hub/sub"

var registered = func() bool { return register("builtin") }()

func BuiltinCount() int { return sub.BuiltinCount }
