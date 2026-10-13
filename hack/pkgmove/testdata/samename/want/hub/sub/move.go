package sub

// Alpha, Beta, Gamma and Delta all have an unexported count method.
type Alpha struct{}

func (Alpha) Count() int { return 1 }

type Beta struct{}

func (Beta) Count() int { return 2 }

type Gamma struct{}

func (Gamma) Count() int { return 3 }

type Delta struct{}

func (Delta) Count() int { return 4 }
