package sub

func DefaultHook() string { return "default" }

// hook can be replaced in tests; it starts as defaultHook.
var Hook = DefaultHook
