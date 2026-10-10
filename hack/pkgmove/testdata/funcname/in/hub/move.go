package hub

func defaultHook() string { return "d" }

type T struct{}

func (T) run() string { return "r" }
