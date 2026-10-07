package gui

// Icons are glyphs of Font Awesome Free 6 Solid (fonts/fa-solid-900.ttf, SIL
// OFL 1.1 — see fonts/LICENSE-fontawesome.txt), merged into the text font so
// they render inline in any label. Codepoints are from Font Awesome's own
// metadata; icons_test.go checks every actions.Icons name has one here.

// actionGlyphs maps every semantic icon name in actions.Icons to a glyph.
var actionGlyphs = map[string]rune{
	"play":     0xf04b, // play
	"pause":    0xf04c, // pause
	"stop":     0xf04d, // stop
	"restart":  0xf2f9, // rotate-right
	"power":    0xf011, // power-off
	"download": 0xf019, // download
	"upgrade":  0xf0aa, // circle-arrow-up
	"logs":     0xf15c, // file-lines
	"info":     0xf05a, // circle-info
	"eye":      0xf06e, // eye
	"doctor":   0xf0f1, // stethoscope
	"wrench":   0xf0ad, // wrench
	"terminal": 0xf120, // terminal
	"gear":     0xf013, // gear
	"plus":     0xf067, // plus
	"list":     0xf03a, // list
	"box":      0xf466, // box
	"hash":     0xf292, // hashtag
	"refresh":  0xf021, // arrows-rotate
	"check":    0xf00c, // check
	"backup":   0xf0c7, // floppy-disk
	"restore":  0xf1da, // clock-rotate-left
	"trash":    0xf1f8, // trash
	"broom":    0xf51a, // broom
	"link":     0xf0c1, // link
	"unlink":   0xf127, // link-slash
	"shield":   0xf3ed, // shield-halved
	"globe":    0xf0ac, // globe
	"onion":    0xf21b, // user-secret
	"i2p":      0xf6fa, // mask
	"mesh":     0xe4e2, // circle-nodes
	"tag":      0xf02b, // tag
}

// glyphGeneric is shown for an icon name this front end does not know yet, so
// a new registry icon degrades to a neutral glyph instead of a blank.
const glyphGeneric = "" // bolt

// UI glyphs that are not action icons.
const (
	glyphServices  = "" // server
	glyphCatalog   = "" // store
	glyphNetwork   = "" // network-wired
	glyphBackups   = "" // box-archive
	glyphHealth    = "" // heart-pulse
	glyphSettings  = "" // sliders
	glyphSearch    = "" // magnifying-glass
	glyphClose     = "" // xmark
	glyphCopy      = "" // copy
	glyphExternal  = "" // up-right-from-square
	glyphClear     = "" // eraser
	glyphWarn      = "" // triangle-exclamation
	glyphOK        = "" // circle-check
	glyphFail      = "" // circle-xmark
	glyphStack     = "" // layer-group
	glyphCubes     = "" // cubes
	glyphDatabase  = "" // database
	glyphCircle    = "" // circle
	glyphClock     = "" // clock
	glyphCommand   = "" // terminal
	glyphChevronDn = "" // chevron-down
	glyphChevronUp = "" // chevron-up
	glyphFolder    = "" // folder-open
	glyphKey       = "" // key
	glyphSave      = "" // floppy-disk
	glyphUndo      = "" // arrow-rotate-left
	glyphArrowDown = "" // arrow-down
)

// Glyph returns the glyph for an action icon name, or a generic one.
func Glyph(icon string) string {
	if r, ok := actionGlyphs[icon]; ok {
		return string(r)
	}
	return glyphGeneric
}

// layerGlyph is the glyph of a network layer, by layer name.
func layerGlyph(layer string) string {
	switch layer {
	case "ts":
		return Glyph("shield")
	case "cf":
		return Glyph("globe")
	case "tor":
		return Glyph("onion")
	case "i2p":
		return Glyph("i2p")
	case "ygg":
		return Glyph("mesh")
	}
	return Glyph("link")
}
