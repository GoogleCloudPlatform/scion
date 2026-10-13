package hub

func mkPolicy(name string) *policy { return newPolicy(name, true) }

const testAction = "read"
