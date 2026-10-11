package hub

//go:generate echo hi

const plain = "p"

func Plain() string { return plain }

func otherLimit() int { return 20 }
