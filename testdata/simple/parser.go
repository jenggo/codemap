package simple

// Parser parses input and produces output.
type Parser struct {
	input string
}

// NewParser creates a new Parser.
func NewParser(input string) *Parser {
	return &Parser{input: input}
}

// Parse processes the input and returns a result.
func (p *Parser) Parse() (string, error) {
	return p.input, nil
}

// Helper is an unexported helper function.
func helper(s string) string {
	return s
}
