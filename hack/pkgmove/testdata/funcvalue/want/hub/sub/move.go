package sub

func DefaultHook() string { return "default" }

// Hook can be replaced in tests; it starts as DefaultHook.
var Hook = DefaultHook
