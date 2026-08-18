package foo

type Service struct{}

func (s *Service) Run() {}

func New() *Service { return &Service{} }
