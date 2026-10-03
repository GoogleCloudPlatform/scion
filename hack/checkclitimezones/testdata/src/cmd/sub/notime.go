package sub

// time is a local name here, not the time package: no zoneless-const.
type clock struct{ Kitchen string }

func notime() string {
	time := clock{Kitchen: "x"}
	return time.Kitchen
}
