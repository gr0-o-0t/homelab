// Package session runs the homelab stack as part of the user's login session
// rather than from Docker's restart policies.
//
// Docker starts `restart: always` containers at boot, before the user logs in.
// When the home directory is encrypted (ecryptfs, fscrypt) or otherwise
// mounted late, every bind mount into the config dir then points at an empty
// placeholder Docker created in the unmounted directory: single-file mounts
// fail ("mounting a directory onto a file"), directory mounts silently serve
// nothing. Session mode turns the restart policies off and lets a systemd user
// service — started at login, stopped at logout — bring the stack up from a
// recorded desired state and restart crashed containers itself.
package session

import "github.com/groot/homelab/internal/desired"

// The desired state moved to internal/desired (the agent reads it too). These
// aliases keep the rest of this package — removed with session mode — as it
// was.
type (
	State   = desired.State
	Desired = desired.Desired
)

const (
	Running = desired.Running
	Stopped = desired.Stopped
	Removed = desired.Removed
)

var (
	LoadDesired    = desired.LoadDesired
	StateDir       = desired.StateDir
	DesiredFile    = desired.DesiredFile
	SharedServices = desired.SharedServices
	WriteAtomic    = desired.WriteAtomic
)
