package cmd

import (
	"fmt"

	"github.com/groot/homelab/internal/service"
	"github.com/groot/homelab/internal/tui/styles"
)

// State formatting shared by the status views: merging a container's state with
// its health into one column.

// mergedCoreState returns a styled state string that merges container state
// with Docker healthcheck status for per-container rows.
func mergedCoreState(state, health string) string {
	switch {
	case state != "running":
		return styles.StateTag(state)
	case health == "healthy":
		return styles.Success.Render("healthy")
	case health == "starting":
		return styles.Warning.Render("starting")
	case health == "unhealthy":
		return styles.Err.Render("unhealthy")
	default:
		return styles.Success.Render("running")
	}
}

// mergedState returns a styled state string that merges container state
// with Docker healthcheck status. Used by the services sub-table.
//
// Returns one of: healthy, running, starting, unhealthy, restarting,
// stopped, partial (N/M).
func mergedState(svc service.Service, health string) string {
	if svc.StateUnknown {
		return styles.Warning.Render("unknown")
	}
	// Check container-level states first for states that Running count can't distinguish.
	for i := range svc.Containers {
		if svc.Containers[i].State == "restarting" {
			return styles.Warning.Render("restarting")
		}
	}

	switch {
	case svc.Total == 0:
		return styles.Muted.Render("stopped")
	case svc.Running == 0:
		return styles.Muted.Render("stopped")
	case svc.Running < svc.Total:
		return styles.Warning.Render(fmt.Sprintf("partial (%d/%d)", svc.Running, svc.Total))
	case health == "healthy":
		return styles.Success.Render("healthy")
	case health == "starting":
		return styles.Warning.Render("starting")
	case health == "unhealthy":
		return styles.Err.Render("unhealthy")
	default:
		return styles.Success.Render("running")
	}
}
