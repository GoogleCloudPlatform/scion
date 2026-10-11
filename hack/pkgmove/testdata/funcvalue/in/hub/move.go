package hub

func defaultHook() string { return "default" }

// hook can be replaced in tests; it starts as defaultHook.
var hook = defaultHook
