// Package actions is the one description of what a user can do with homelab,
// shared by the terminal dashboard and the desktop GUI.
//
// Every action is a CLI invocation: Args returns the argv after the binary,
// and a front end runs it through the same binary (see cmd.selfCLI). A front
// end therefore renders buttons and menus from this list instead of keeping
// its own, and cannot disagree with the CLI about what an action does. A
// parity test in cmd fails when a CLI command or flag has no action here and
// is not on its commented exclusion list.
package actions

import (
	"slices"
	"sort"
	"strings"

	"github.com/groot/homelab/internal/service"
)

// Scope is the kind of thing an action is offered on.
type Scope int

const (
	// Global actions concern the whole stack and take no target: overview,
	// batch operations over all services or a group, scaffolding, version.
	Global Scope = iota
	// Core actions act on the core stack (Tailscale + Caddy + enabled
	// extensions) — the CLI's no-service forms.
	Core
	// Service actions act on one installed service.
	Service
	// CatalogEntry actions act on a catalog service that is not installed.
	CatalogEntry
	// Layer actions act on one optional network extension (cf, tor, i2p, ygg).
	Layer
)

func (s Scope) String() string {
	return [...]string{"global", "core", "service", "catalog", "layer"}[s]
}

// Danger says how a front end must confirm an action before running it.
type Danger int

const (
	// None runs on a click.
	None Danger = iota
	// Confirm asks yes/no first. Help says what is at stake.
	Confirm
	// TypeName makes the user type Action.Token(target) first: the action
	// deletes data or configuration.
	TypeName
)

// Groups, in display order. For returns actions sorted by this order.
const (
	GroupLifecycle   = "Lifecycle"
	GroupExposure    = "Exposure"
	GroupInspect     = "Inspect"
	GroupMaintenance = "Maintenance"
	GroupConfigure   = "Configure"
	GroupDanger      = "Danger zone"
)

// Groups lists every group in display order.
var Groups = []string{GroupLifecycle, GroupExposure, GroupInspect, GroupMaintenance, GroupConfigure, GroupDanger}

// Icons is every semantic icon name an action uses. Each front end maps all
// of them to its own glyph; a test checks no action uses one outside this set.
var Icons = []string{
	"play", "pause", "stop", "restart", "power", "download", "upgrade",
	iconLogs, "info", "eye", "doctor", "wrench", "terminal", "gear", "plus",
	"list", "box", "hash", "refresh", "check", "backup", "restore", "trash",
	"broom", "link", "unlink", "shield", "globe", "onion", "i2p", "mesh",
	"tag",
}

const iconLogs = "logs"

// InputKind is how a front end collects an Input.
type InputKind int

const (
	Text   InputKind = iota // free text
	Bool                    // checkbox; value "true" or ""
	Path                    // a filesystem path (file picker / text)
	Choice                  // one of Choices, or of the dynamic Source
)

// Dynamic choice sources; resolve them with Choices.
const (
	SourceGroups  = "groups"  // service groups from the root config.yaml
	SourceBackups = "backups" // backup directories, newest first
)

// Input is an extra parameter an action needs beyond its target.
type Input struct {
	Key      string
	Label    string
	Kind     InputKind
	Default  string
	Required bool
	Choices  []string // static options for Choice
	Source   string   // dynamic options for Choice: SourceGroups, SourceBackups
	Help     string
}

// Inputs are the collected input values by Input.Key. Bools are "true" or "".
type Inputs map[string]string

// Bool reports whether a Bool input is set.
func (in Inputs) Bool(key string) bool { return in[key] == "true" }

// Target is what an action is run on, with the state Available looks at.
type Target struct {
	Scope Scope
	// Name is the service, catalog entry or layer name; "" for Core and Global.
	Name string
	// Names is a multi-selection of services, for Multi actions: Build passes
	// all of them in one invocation. Running/Total/Exposed then describe the
	// selection as the front end sees fit (e.g. summed).
	Names []string
	// Running and Total count containers: a service's, or a layer's own one.
	Running, Total int
	// Exposed lists the layers a service is exposed on (service.Service.Layers).
	Exposed []string
	// Layers are the layers the stack offers: the private one ("ts") plus every
	// extension enabled in config.yaml. Gates the per-layer exposure actions.
	Layers []string
	// Enabled reports, for a Layer target, that the extension is enabled in
	// config.yaml.
	Enabled bool
}

// ServiceTarget is the target for a discovered service: Service scope when it
// is installed, CatalogEntry otherwise. layers is as for Target.Layers.
func ServiceTarget(s service.Service, layers []string) Target {
	t := Target{Scope: Service, Name: s.Name, Running: s.Running, Total: s.Total, Exposed: s.Layers, Layers: layers}
	if !s.Installed {
		t.Scope = CatalogEntry
	}
	return t
}

// CoreTarget is the core stack row.
func CoreTarget(layers []string) Target { return Target{Scope: Core, Layers: layers} }

// GlobalTarget is the stack as a whole.
func GlobalTarget(layers []string) Target { return Target{Scope: Global, Layers: layers} }

// LayerTarget is one optional network extension.
func LayerTarget(name string, enabled, running bool) Target {
	t := Target{Scope: Layer, Name: name, Enabled: enabled, Total: 1}
	if running {
		t.Running = 1
	}
	return t
}

// names are the service names an invocation acts on.
func (t Target) names() []string {
	if len(t.Names) > 0 {
		return t.Names
	}
	if t.Name != "" {
		return []string{t.Name}
	}
	return nil
}

func (t Target) offers(layer string) bool { return slices.Contains(t.Layers, layer) }
func (t Target) exposed(layer string) bool {
	return slices.Contains(t.Exposed, layer)
}

// Action is one user-facing capability, expressed as a CLI invocation.
type Action struct {
	ID    string // stable, unique: "service.up", "service.enable.cf", …
	Label string
	Group string // one of Groups
	Icon  string // one of Icons
	Help  string // one line: tooltip, and the text of a confirmation dialog
	Scope Scope

	// Args returns the argv after the binary. Call Build, which fills input
	// defaults first.
	Args   func(Target, Inputs) []string
	Inputs []Input

	// Danger is how to confirm. Args already passes the CLI's --yes where the
	// CLI would otherwise prompt, so the front end's confirmation is the only one.
	Danger Danger
	// Interactive actions need a real terminal (prompts, a shell, a wizard):
	// the TUI suspends itself (tea.ExecProcess); the GUI opens a terminal.
	Interactive bool
	// Multi actions accept several services at once (Target.Names): the CLI
	// takes `<verb> a b c`.
	Multi bool
	// Stream actions keep printing until stopped (logs -f): show the output
	// live and offer a way to stop the process.
	Stream bool

	// Available reports whether the action makes sense for the target's
	// current state; nil means always.
	Available func(Target) bool

	// Commands lists the cobra command paths (without "homelab") and flags
	// ("up --build") this action exercises, for the parity test.
	Commands []string
}

// Build returns the argv for running the action on t, with every input not
// given in in set to its default.
func (a Action) Build(t Target, in Inputs) []string {
	t = a.single(t)
	full := Inputs{}
	for _, i := range a.Inputs {
		full[i.Key] = i.Default
	}
	for k, v := range in {
		full[k] = v
	}
	return a.Args(t, full)
}

// single folds a one-service selection into Name for an action that is not
// Multi, so it never runs its no-service (core stack) form by accident.
func (a Action) single(t Target) Target {
	if !a.Multi {
		if t.Name == "" && len(t.Names) == 1 {
			t.Name = t.Names[0]
		}
		t.Names = nil
	}
	return t
}

// Can reports whether the action applies to t: same scope, Available, and
// Multi when several services are selected.
func (a Action) Can(t Target) bool {
	if len(t.Names) > 1 && !a.Multi {
		return false
	}
	return a.Scope == t.Scope && (a.Available == nil || a.Available(t))
}

// Token is what a TypeName action makes the user type: the service's name,
// or "all" for a batch or a multi-selection — the token the CLI itself asks for.
func (a Action) Token(t Target) string {
	t = a.single(t)
	if names := t.names(); len(names) == 1 {
		return names[0]
	}
	return "all"
}

var registry = build()

// All returns every action, in registry order.
func All() []Action { return slices.Clone(registry) }

// ByID returns the action with the given ID.
func ByID(id string) (Action, bool) {
	for i := range registry {
		if registry[i].ID == id {
			return registry[i], true
		}
	}
	return Action{}, false
}

// For returns the actions applicable to t, ordered by group (see Groups) and
// then registry order.
func For(t Target) []Action {
	var out []Action
	for i := range registry {
		if registry[i].Can(t) {
			out = append(out, registry[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return groupRank(out[i].Group) < groupRank(out[j].Group) })
	return out
}

func groupRank(g string) int { return slices.Index(Groups, g) }

// fields splits a command line on spaces, for the exec action's command.
func fields(s string) []string { return strings.Fields(s) }
