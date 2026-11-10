package p

type Server struct{}

type List[T any] struct{}

func (s *Server) handle() {}

func (l *List[T]) handle() {}

func handle() {}

func init() {}

var _ = handle
