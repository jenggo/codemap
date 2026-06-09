package embedding

// Base provides common functionality.
type Base struct {
	ID int
}

// GetName returns the base name.
func (b *Base) GetName() string {
	return "base"
}

// Extended embeds Base and adds more functionality.
type Extended struct {
	Base
	Name string
}

// GetFullName returns the full name combining base and extended.
func (e *Extended) GetFullName() string {
	return e.GetName() + "." + e.Name
}

// AnotherBase provides different common functionality.
type AnotherBase struct {
	Value int
}

// GetValue returns the base value.
func (a *AnotherBase) GetValue() int {
	return a.Value
}

// MultiEmbed embeds both Base and AnotherBase.
type MultiEmbed struct {
	Base
	AnotherBase
	Label string
}

// GetInfo returns combined information from all embedded types.
func (m *MultiEmbed) GetInfo() string {
	return m.GetName() + ": " + m.Label
}
