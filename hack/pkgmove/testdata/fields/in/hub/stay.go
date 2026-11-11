package hub

func Make() *Job { return &Job{name: "n"} }

func Name(j *Job) string { return j.name }
