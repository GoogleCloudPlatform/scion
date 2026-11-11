package hub

func helper() int { return 1 }

// Other is already exported, so helper=Other collides.
func Other() int { return 2 }

type item struct{}

func (item) label() string { return "x" }
