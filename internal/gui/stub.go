//go:build nogui

package gui

import "errors"

// Run reports that this is a headless build.
func Run(Options) error {
	return errors.New("this is a headless homelab build without the desktop GUI — use the " +
		"standard build (`make build`, or the release binary without _headless) on a desktop")
}
