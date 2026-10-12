package hub

// Alpha, Beta, Gamma and Delta all have an unexported count method.
type Alpha struct{}

func (Alpha) count() int { return 1 }

type Beta struct{}

func (Beta) count() int { return 2 }

type Gamma struct{}

func (Gamma) count() int { return 3 }

type Delta struct{}

func (Delta) count() int { return 4 }
