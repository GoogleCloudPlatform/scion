package sub

//go:generate echo hi

const plain = "p"

func Plain() string { return plain }

func OtherLimit() int { return 20 }
