package hub

func fooBar() int { return 1 }

func FooBar() int { return 2 }

func compute() int { return 3 }

func shadow() int {
	Compute := 4
	return Compute + compute()
}
