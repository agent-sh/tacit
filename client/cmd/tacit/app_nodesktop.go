//go:build !desktop

package main

import "errors"

// desktopBuild says whether this binary has the desktop window. The default
// build does not: it stays pure Go, so the hooks every session runs do not
// load GTK and WebKit. Build with -tags desktop,production,webkit2_41 (Linux)
// or -tags desktop,production (macOS) for `tacit app`.
const desktopBuild = false

// appIconICNS is only in the desktop build, which is the one that installs the app.
var appIconICNS []byte

func runDesktop(*App) error {
	return errors.New("this tacit was built without the desktop window")
}
