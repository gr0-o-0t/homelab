package gui

import (
	"slices"
	"strconv"
	"strings"

	"github.com/groot/homelab/internal/actions"
	"github.com/groot/homelab/internal/service"
)

// This file is the GUI's pure logic: which actions a screen shows where. It
// is built without the gui tag so `go test ./...` covers it. Nothing here
// keeps an action list of its own — every button comes from actions.For, so a
// new CLI action appears in the GUI without a change here.

// surface splits actions.For(t) over a detail pane: the toolbar (lifecycle,
// update, logs), the exposure controls of the Network tab, and the rest,
// shown grouped in the Actions tab. Every action of For(t) lands in exactly
// one of them.
type surface struct {
	Toolbar []actions.Action
	Network []actions.Action
	Rest    []actions.Action
}

// onToolbar reports whether an action belongs on a detail pane's primary
// toolbar: container lifecycle, update, and the logs stream.
func onToolbar(a actions.Action) bool {
	if a.Danger != actions.None {
		return false
	}
	switch {
	case a.Group == actions.GroupLifecycle:
		return true
	case a.Icon == "upgrade":
		return true
	case a.Stream && a.Icon == "logs" && strings.HasSuffix(a.ID, ".logs") && strings.Count(a.ID, ".") == 1:
		return true // service.logs / core.logs, not caddy.logs etc.
	}
	return false
}

func split(t actions.Target) surface {
	var s surface
	targetActs := actions.For(t)
	for i := range targetActs {
		a := &targetActs[i]
		switch {
		case onToolbar(*a):
			s.Toolbar = append(s.Toolbar, *a)
		case a.Group == actions.GroupExposure && t.Scope == actions.Service:
			s.Network = append(s.Network, *a)
		default:
			s.Rest = append(s.Rest, *a)
		}
	}
	return s
}

// actionGroup is one Groups heading and its actions.
type actionGroup struct {
	Name    string
	Actions []actions.Action
}

// grouped buckets actions by Group, in actions.Groups order; empty groups are
// left out. An unknown group sorts last under its own name.
func grouped(list []actions.Action) []actionGroup {
	var out []actionGroup
	idx := map[string]int{}
	order := slices.Clone(actions.Groups)
	for i := range list {
		a := &list[i]
		if !slices.Contains(order, a.Group) {
			order = append(order, a.Group)
		}
	}
	for _, name := range order {
		for k := range list {
			a := &list[k]
			if a.Group != name {
				continue
			}
			i, ok := idx[name]
			if !ok {
				i = len(out)
				idx[name] = i
				out = append(out, actionGroup{Name: name})
			}
			out[i].Actions = append(out[i].Actions, *a)
		}
	}
	return out
}

// filterBy keeps the actions for which keep is true.
func filterBy(list []actions.Action, keep func(actions.Action) bool) []actions.Action {
	var out []actions.Action
	for i := range list {
		a := &list[i]
		if keep(*a) {
			out = append(out, *a)
		}
	}
	return out
}

// touchesCommand reports whether a exercises a CLI command starting with verb
// ("backup", "restore") — how the Backups view picks its actions.
func touchesCommand(a actions.Action, verbs ...string) bool {
	for _, c := range a.Commands {
		first, _, _ := strings.Cut(c, " ")
		if slices.Contains(verbs, first) {
			return true
		}
	}
	return false
}

// multiTarget is the selection of several services as one Service target:
// container counts summed, exposure the layers every selected service has.
func multiTarget(all []service.Service, names, layers []string) actions.Target {
	t := actions.Target{Scope: actions.Service, Names: slices.Clone(names), Layers: layers}
	first := true
	for i := range all {
		s := &all[i]
		if !slices.Contains(names, s.Name) {
			continue
		}
		t.Running += s.Running
		t.Total += s.Total
		var exposed []string
		for _, l := range s.Layers {
			exposed = append(exposed, string(l))
		}
		if first {
			t.Exposed, first = exposed, false
			continue
		}
		t.Exposed = slices.DeleteFunc(t.Exposed, func(l string) bool { return !slices.Contains(exposed, l) })
	}
	return t
}

// layerToggle finds the actions that expose t on layer and hide it again:
// "service.enable" / "service.disable" for the tailnet, ".<layer>" suffixed
// for an extension. on reports t is exposed there; act is the action a toggle
// click runs (nil when the layer cannot be toggled for t right now).
func layerToggle(t actions.Target, layer string) (on bool, act *actions.Action) {
	on = slices.Contains(t.Exposed, layer)
	suffix := ""
	if layer != "ts" {
		suffix = "." + layer
	}
	id := "service.enable" + suffix
	if on {
		id = "service.disable" + suffix
	}
	acts := actions.For(t)
	for i := range acts {
		a := &acts[i]
		if a.ID == id {
			return on, a
		}
	}
	return on, nil
}

// layerToggleIDs are the action IDs the per-layer toggles consume, so the
// Network tab can list the remaining exposure actions separately.
func layerToggleIDs(layers []string) []string {
	var ids []string
	for _, l := range layers {
		suffix := ""
		if l != "ts" {
			suffix = "." + l
		}
		ids = append(ids, "service.enable"+suffix, "service.disable"+suffix)
	}
	return ids
}

// needsForm reports whether running a from a button must open the input /
// confirmation modal first. quick is a click on a toolbar button, where
// optional inputs keep their defaults (right-click opens the form instead).
func needsForm(a actions.Action, quick bool) bool {
	if a.Danger != actions.None {
		return true
	}
	if quick {
		for _, in := range a.Inputs {
			if in.Required {
				return true
			}
		}
		return false
	}
	return len(a.Inputs) > 0
}

// missingInputs lists the labels of required inputs that are empty.
func missingInputs(a actions.Action, values actions.Inputs) []string {
	var out []string
	for _, in := range a.Inputs {
		if in.Required && strings.TrimSpace(values[in.Key]) == "" {
			out = append(out, in.Label)
		}
	}
	return out
}

// defaultInputs is the form's starting values: each input's default.
func defaultInputs(a actions.Action) actions.Inputs {
	out := actions.Inputs{}
	for _, in := range a.Inputs {
		out[in.Key] = in.Default
	}
	return out
}

// stateOf summarises a service's container state for its dot and pill.
type runState int

const (
	stateAvailable runState = iota // catalog entry
	stateStopped
	statePartial
	stateRunning
)

func serviceState(s service.Service) runState {
	switch {
	case !s.Installed:
		return stateAvailable
	case s.Total > 0 && s.Running == s.Total:
		return stateRunning
	case s.Running > 0:
		return statePartial
	}
	return stateStopped
}

func (r runState) String() string {
	return [...]string{"available", "stopped", "partial", "running"}[r]
}

// stateLabel is the pill text: "running", "2/3 running", "stopped".
func stateLabel(s service.Service) string {
	if serviceState(s) == statePartial {
		return strconv.Itoa(s.Running) + "/" + strconv.Itoa(s.Total) + " running"
	}
	if serviceState(s) == stateStopped && s.Total == 0 {
		return "not created"
	}
	return serviceState(s).String()
}

// matchesFilter is the services list search: every space-separated word must
// occur in the name or one of the exposure layers.
func matchesFilter(s service.Service, query string) bool {
	hay := strings.ToLower(s.Name)
	for _, l := range s.Layers {
		hay += " " + string(l)
	}
	for _, w := range strings.Fields(strings.ToLower(query)) {
		if !strings.Contains(hay, w) {
			return false
		}
	}
	return true
}

// extState is one optional network extension as the Network view shows it.
type extState struct {
	Name, Label, Container string
	Enabled, Running       bool
}

// screenTargets is every target the GUI renders actions for, given the
// discovered state: the stack (Services view, pinned row), the core and each
// extension (Network view), each installed service and catalog entry, and the
// multi-selection of all installed services (batch bar). The views build
// their targets with the same constructors, and the parity test checks the
// union of their actions is actions.All().
func screenTargets(svcs []service.Service, layers []string, exts []extState) []actions.Target {
	out := []actions.Target{actions.GlobalTarget(layers), actions.CoreTarget(layers)}
	var installed []string
	for i := range svcs {
		s := &svcs[i]
		out = append(out, actions.ServiceTarget(*s, layers))
		if s.Installed {
			installed = append(installed, s.Name)
		}
	}
	for _, e := range exts {
		out = append(out, actions.LayerTarget(e.Name, e.Enabled, e.Running))
	}
	if len(installed) > 1 {
		out = append(out, multiTarget(svcs, installed, layers))
	}
	return out
}
