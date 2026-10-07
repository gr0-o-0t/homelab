package styles

import "os"

// Icons: one glyph per semantic icon name used by internal/actions (see
// actions.Icons) plus the state glyphs the TUI draws itself.
//
// The default set is plain Unicode symbols that every common terminal font
// covers and that occupy exactly one cell — no emoji, whose width differs
// between terminals and breaks column alignment. A test checks each glyph's
// lipgloss.Width. Set HOMELAB_NERD_FONT=1 to use Nerd Font glyphs instead.

var unicodeIcons = map[string]string{
	"play": "▶", "pause": "‖", "stop": "■", "restart": "↻", "power": "⏻",
	"download": "↓", "upgrade": "⇡", "logs": "≡", "info": "ℹ", "eye": "◉",
	"doctor": "✚", "wrench": "⚒", "terminal": "❯", "gear": "⚙", "plus": "+",
	"list": "▤", "box": "▣", "hash": "#", "refresh": "⟳", "check": "✓",
	"backup": "⤓", "restore": "⤒", "trash": "✗", "broom": "⌫", "link": "⇆",
	"unlink": "⇸", "shield": "⛨", "globe": "◍", "onion": "◈", "i2p": "◬",
	"mesh": "⬡", "tag": "⚑",

	// state and chrome
	"running": "●", "partial": "◐", "stopped": "○", "catalog": "◇",
	"core": "◆", "marked": "◆", "unmarked": "◇", "cursor": "▸", "error": "✗",
	"ok": "✓", "warn": "!", "home": "⌂", "search": "›", "secret": "•",
	"backups": "⤓", "health": "✚", "network": "⇆", "services": "▣",
}

var nerdIcons = map[string]string{
	"play": "", "pause": "", "stop": "", "restart": "", "power": "",
	"download": "", "upgrade": "", "logs": "", "info": "", "eye": "",
	"doctor": "", "wrench": "", "terminal": "", "gear": "", "plus": "",
	"list": "", "box": "", "hash": "", "refresh": "", "check": "",
	"backup": "", "restore": "", "trash": "", "broom": "", "link": "",
	"unlink": "", "shield": "", "globe": "", "onion": "", "i2p": "",
	"mesh": "", "tag": "",

	"running": "", "partial": "", "stopped": "", "catalog": "",
	"core": "", "marked": "", "unmarked": "", "cursor": "", "error": "",
	"ok": "", "warn": "", "home": "", "search": "", "secret": "",
	"backups": "", "health": "", "network": "", "services": "",
}

// NerdFont reports whether the Nerd Font glyph set is selected.
func NerdFont() bool { return os.Getenv("HOMELAB_NERD_FONT") == "1" }

// Icon returns the glyph for a semantic icon name; "•" for an unknown one.
func Icon(name string) string {
	set := unicodeIcons
	if NerdFont() {
		set = nerdIcons
	}
	if g, ok := set[name]; ok {
		return g
	}
	return "•"
}

// IconSets returns both glyph sets, for tests.
func IconSets() map[string]map[string]string {
	return map[string]map[string]string{"unicode": unicodeIcons, "nerd": nerdIcons}
}
