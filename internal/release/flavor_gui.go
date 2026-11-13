//go:build !nogui

package release

// guiBuild reports whether this binary carries the desktop GUI: the release
// asset it is replaced from must be the same flavour (linux_amd64, the only
// GUI build).
const guiBuild = true
