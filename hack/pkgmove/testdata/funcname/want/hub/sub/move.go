package sub

func DefaultHook() string { return "d" }

type T struct{}

func (T) Run() string { return "r" }
