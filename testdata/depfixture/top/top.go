package top

import "codemap/testdata/depfixture/middle"

// App is the top-level entry.
type App struct {
	S *middle.Service
}

// Start kicks off the app.
func (a *App) Start() string {
	return a.S.Run()
}
