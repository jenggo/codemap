package pattern

import "fmt"

// Handle does work and reports failures.
func Handle() error {
	defer fmt.Println("done")
	err := work()
	if err != nil {
		return err
	}
	return nil
}

func work() error { return nil }

func same() bool {
	return check() && check()
}

func check() bool { return true }
