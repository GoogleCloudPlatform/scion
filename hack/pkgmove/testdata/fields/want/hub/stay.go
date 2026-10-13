package hub

func Make() *Job { return &Job{Name: "n"} }

func Name(j *Job) string { return j.Name }
