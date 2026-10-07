//go:build !gui

package gui

import "errors"

// Run reports that this binary was built without the GUI.
func Run(Options) error {
	return errors.New("this homelab was built without the GUI — build it with `make gui` " +
		"(needs cgo and the OpenGL/X11 development headers)")
}
