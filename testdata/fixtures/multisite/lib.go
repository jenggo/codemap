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
