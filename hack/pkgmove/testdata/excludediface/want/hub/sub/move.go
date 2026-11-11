package sub

type worker struct{}

func (worker) run() string { return "w" }

func NewWorker() any { return worker{} }
