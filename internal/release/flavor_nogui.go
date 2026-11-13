//go:build nogui

package release

// guiBuild reports whether this binary carries the desktop GUI: a -tags
// nogui build is replaced from a headless asset (linux_amd64_headless or
// linux_arm64).
const guiBuild = false
