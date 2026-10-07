package dashboard

import (
	"github.com/groot/homelab/internal/actions"
)

// shortcut binds one key to one action for a scope. This is the only
// hand-maintained action table in the TUI: everything else — the palette, the
// detail pane's action list, the help overlay — is derived from the registry.
type shortcut struct {
	key string
	id  string
}

// shortcuts are tried for the selection's scope first, then for Global.
var shortcuts = map[actions.Scope][]shortcut{
	actions.Service: {
		{"u", "service.up"}, {"S", "service.start"}, {"s", "service.stop"}, {"r", "service.restart"},
		{"x", "service.down"}, {"U", "service.update"}, {"l", "service.logs"}, {"c", "service.setup"},
		{"t", "service.shell"}, {"b", "service.backup"},
	},
	actions.Core: {
		{"u", "core.up"}, {"S", "core.start"}, {"s", "core.stop"}, {"r", "core.restart"},
		{"x", "core.down"}, {"U", "core.update"}, {"l", "core.logs"}, {"v", "core.validate"},
	},
	actions.CatalogEntry: {{"i", "catalog.add"}},
	actions.Layer: {
		{"e", "layer.enable"}, {"d", "layer.disable"}, {"u", "layer.start"}, {"s", "layer.stop"},
		{"l", "layer.logs"},
	},
	actions.Global: {{"n", "global.new"}, {"b", "global.backup"}, {"r", "global.restore"}},
}

// shortcutFor returns the action a key runs for a scope.
func shortcutFor(sc actions.Scope, key string) (actions.Action, bool) {
	for _, s := range shortcuts[sc] {
		if s.key == key {
			return actions.ByID(s.id)
		}
	}
	return actions.Action{}, false
}

// keyFor is the shortcut key of an action, "" when it has none.
func keyFor(id string) string {
	for _, list := range shortcuts {
		for _, s := range list {
			if s.id == id {
				return s.key
			}
		}
	}
	return ""
}

// navKeys are the keys that are not actions, for the help overlay.
var navKeys = [][2]string{
	{"1-5 tab", "switch view (shift+tab back)"},
	{"↑↓ j k", "move"},
	{"gg G", "top / bottom"},
	{"^u ^d", "half page"},
	{"enter", "actions for the selection"},
	{": ^p", "command palette"},
	{"e", "exposure menu (service)"},
	{"space", "mark service (multi-select)"},
	{"A", "mark all · esc clears"},
	{"/", "filter list"},
	{"o", "last command output"},
	{"J K", "scroll Health output"},
	{"R", "refresh now"},
	{"?", "this help"},
	{"q ^c", "quit"},
}
