package samepair

type Service struct{ x int }

func NewService() *Service { return &Service{} }

func (s *Service) Get() int { return s.x }

var Global = func() int { return 0 }
