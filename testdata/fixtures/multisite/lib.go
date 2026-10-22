package multisite

func Target() int { return 0 }

func Helper() int {
	_ = Target()
	_ = Target()
	_ = Target()
	return Target()
}

func Other() {
	_ = Target()
}

// SameLine calls Target twice on one line, so the edge carries two sites that
// differ only by column.
func SameLine() int {
	return Target() + Target()
}
