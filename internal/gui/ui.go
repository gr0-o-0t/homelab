//go:build !nogui

package gui

import (
	"image/color"
	"math"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	g "github.com/AllenDang/giu"
)

// Small immediate-mode building blocks on top of Dear ImGui. Everything here
// runs on the UI thread and reads only the frame snapshot.

var (
	cText    = hexColor(hexText)
	cMuted   = hexColor(hexComment)
	cDim     = hexColor(hexMuted)
	cPrimary = hexColor(hexPrimary)
	cBlue    = hexColor(hexBlue)
	cSuccess = hexColor(hexSuccess)
	cWarning = hexColor(hexWarning)
	cError   = hexColor(hexError)
	cAccent  = hexColor(hexAccent)
	cBorder  = hexColor(hexBorder)
	cBgDark  = hexColor(hexBgDark)
	cPanel   = hexColor(hexBgPanel)
	cRaised  = hexColor(hexBgRaised)
	cHover   = hexColor(hexBgHover)
	cActive  = hexColor(hexBgActive)
)

func colorOf(c rgba) color.Color {
	return color.RGBA{uint8(c.R * 255), uint8(c.G * 255), uint8(c.B * 255), uint8(c.A * 255)}
}

func u32(c rgba) uint32 { return imgui.ColorConvertFloat4ToU32(vec4(c)) }

func v2(x, y float32) imgui.Vec2 { return imgui.Vec2{X: x, Y: y} }

// esc makes s safe as an ImGui format string.
func esc(s string) string { return strings.ReplaceAll(s, "%", "%%") }

func text(s string) { imgui.TextUnformatted(s) }

func textC(c rgba, s string) {
	imgui.PushStyleColorVec4(imgui.ColText, vec4(c))
	imgui.TextUnformatted(s)
	imgui.PopStyleColor()
}

func textWrapped(c rgba, s string) {
	imgui.PushStyleColorVec4(imgui.ColText, vec4(c))
	imgui.PushTextWrapPosV(0)
	imgui.TextUnformatted(s)
	imgui.PopTextWrapPos()
	imgui.PopStyleColor()
}

// tip shows s as the last item's tooltip after a short hover.
func tip(s string) {
	if s != "" && imgui.IsItemHoveredV(imgui.HoveredFlagsDelayShort|imgui.HoveredFlagsAllowWhenDisabled) {
		imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(10, 8))
		if imgui.BeginTooltip() {
			imgui.PushTextWrapPosV(imgui.FontSize() * 28)
			imgui.TextUnformatted(s)
			imgui.PopTextWrapPos()
			imgui.EndTooltip()
		}
		imgui.PopStyleVar()
	}
}

// withFont renders fn in font (nil = current) at size (0 = current).
func withFont(font *g.FontInfo, size float32, fn func()) {
	pushed := false
	if font != nil {
		pushed = g.PushFont(font)
	}
	sized := false
	if size > 0 {
		sized = g.PushFontSize(size * imgui.CurrentStyle().FontScaleDpi())
	}
	fn()
	if sized {
		g.PopFont()
	}
	if pushed {
		g.PopFont()
	}
}

func heading(s string, size float32) { withFont(fontBold, size, func() { text(s) }) }

// ── buttons ───────────────────────────────────────────────────────────────────

type tone int

const (
	toneNormal tone = iota
	tonePrimary
	toneSuccess
	toneWarning
	toneDanger
	toneAccent
	toneGhost
)

func (t tone) colors() (base, hover, active, fg rgba) {
	tint := func(c rgba) (rgba, rgba, rgba, rgba) {
		return c.alpha(0.16), c.alpha(0.28), c.alpha(0.40), c
	}
	switch t {
	case tonePrimary:
		return tint(cPrimary)
	case toneSuccess:
		return tint(cSuccess)
	case toneWarning:
		return tint(cWarning)
	case toneDanger:
		return tint(cError)
	case toneAccent:
		return tint(cAccent)
	case toneGhost:
		return rgba{}, cHover, cActive, cText
	}
	return cRaised, cHover, cActive, cText
}

// button is a tinted button; label may carry a ##id suffix.
func button(label string, t tone, disabled bool) bool {
	base, hover, active, fg := t.colors()
	imgui.PushStyleColorVec4(imgui.ColButton, vec4(base))
	imgui.PushStyleColorVec4(imgui.ColButtonHovered, vec4(hover))
	imgui.PushStyleColorVec4(imgui.ColButtonActive, vec4(active))
	imgui.PushStyleColorVec4(imgui.ColText, vec4(fg))
	imgui.BeginDisabledV(disabled)
	clicked := imgui.Button(label)
	imgui.EndDisabled()
	if imgui.IsItemHovered() && !disabled {
		imgui.SetMouseCursor(imgui.MouseCursorHand)
	}
	imgui.PopStyleColorV(4)
	return clicked && !disabled
}

// toneFor colours an action button by what it does.
func toneForIcon(icon string) tone {
	switch icon {
	case "play", "power":
		return toneSuccess
	case "pause":
		return toneWarning
	case "stop", "trash", "unlink":
		return toneDanger
	case "restart", "refresh", "download":
		return tonePrimary
	case "upgrade", "backup", "restore", "plus":
		return toneAccent
	}
	return toneNormal
}

// flow lays buttons out left to right, wrapping at the region's edge.
type flow struct {
	right float32 // screen x of the region's right edge
	first bool
}

func newFlow() *flow {
	return &flow{right: imgui.CursorScreenPos().X + imgui.ContentRegionAvail().X, first: true}
}

// next positions the next item of width w (label width computed when w == 0).
func (f *flow) next(label string, w float32) {
	if w == 0 {
		w = buttonWidth(label)
	}
	if !f.first {
		imgui.SameLine()
		if imgui.CursorScreenPos().X+w > f.right {
			imgui.NewLine()
		}
	}
	f.first = false
}

func buttonWidth(label string) float32 {
	pad := imgui.CurrentStyle().FramePadding()
	return imgui.CalcTextSizeV(label, true, -1).X + 2*pad.X
}

// ── indicators ────────────────────────────────────────────────────────────────

// dot draws a status dot inline, vertically centred on the text line.
func dot(c rgba) { dotH(c, imgui.TextLineHeight()) }

// dotF is dot on a line aligned to framed widgets (after a checkbox, or
// AlignTextToFramePadding).
func dotF(c rgba) { dotH(c, imgui.FrameHeight()) }

func dotH(c rgba, h float32) {
	p := imgui.CursorScreenPos()
	r := h * 0.22
	imgui.WindowDrawList().AddCircleFilled(v2(p.X+r+1, p.Y+h/2+1), r, u32(c))
	imgui.Dummy(v2(2*r+2, h))
}

// pill draws a rounded badge with text in colour c.
func pill(s string, c rgba) {
	pad := v2(8, 2)
	sz := imgui.CalcTextSize(s)
	p := imgui.CursorScreenPos()
	off := (imgui.FrameHeight() - sz.Y - 2*pad.Y) / 2
	if off < 0 {
		off = 0
	}
	dl := imgui.WindowDrawList()
	min := v2(p.X, p.Y+off)
	max := v2(p.X+sz.X+2*pad.X, p.Y+off+sz.Y+2*pad.Y)
	dl.AddRectFilledV(min, max, u32(c.alpha(0.16)), (max.Y-min.Y)/2, imgui.DrawFlagsNone)
	dl.AddTextVec2(v2(min.X+pad.X, min.Y+pad.Y), u32(c), s)
	imgui.Dummy(v2(max.X-min.X, imgui.FrameHeight()))
}

// spinner draws a rotating arc of the given radius.
func spinner(r float32, c rgba) {
	p := imgui.CursorScreenPos()
	h := imgui.TextLineHeight()
	center := v2(p.X+r+1, p.Y+h/2)
	t := float32(imgui.Time()) * 6
	dl := imgui.WindowDrawList()
	dl.PathClear()
	dl.PathArcToV(center, r, t, t+float32(math.Pi)*1.4, 20)
	dl.PathStrokeV(u32(c), imgui.DrawFlagsNone, r*0.35)
	imgui.Dummy(v2(2*r+2, h))
}

// toggle draws an on/off switch; it returns true when clicked.
func toggle(id string, on, disabled bool) bool {
	h := imgui.FrameHeight() * 0.8
	w := h * 1.8
	p := imgui.CursorScreenPos()
	y := p.Y + (imgui.FrameHeight()-h)/2
	clicked := imgui.InvisibleButtonV(id, v2(w, imgui.FrameHeight()), imgui.ButtonFlagsNone)
	hovered := imgui.IsItemHovered()
	if hovered && !disabled {
		imgui.SetMouseCursor(imgui.MouseCursorHand)
	}
	track := cBorder
	if on {
		track = cSuccess.alpha(0.8)
	}
	if hovered && !disabled {
		track = track.alpha(1)
		if !on {
			track = cMuted
		}
	}
	if disabled {
		track = track.alpha(0.35)
	}
	dl := imgui.WindowDrawList()
	dl.AddRectFilledV(v2(p.X, y), v2(p.X+w, y+h), u32(track), h/2, imgui.DrawFlagsNone)
	knob := p.X + h/2
	if on {
		knob = p.X + w - h/2
	}
	kc := cText
	if disabled {
		kc = cDim
	}
	dl.AddCircleFilled(v2(knob, y+h/2), h/2-3, u32(kc))
	return clicked && !disabled
}

// link renders a clickable URL: click opens it, right-click copies it.
func (a *app) link(label, url string) {
	textC(cPrimary, label)
	if imgui.IsItemHovered() {
		imgui.SetMouseCursor(imgui.MouseCursorHand)
		min, max := imgui.ItemRectMin(), imgui.ItemRectMax()
		imgui.WindowDrawList().AddLine(v2(min.X, max.Y), v2(max.X, max.Y), u32(cPrimary))
	}
	tip(url + "\nClick to open · right-click to copy")
	if imgui.IsItemClicked() {
		a.openURL(url)
	}
	if imgui.IsItemClickedV(imgui.MouseButtonRight) {
		imgui.SetClipboardText(url)
		a.notify("Copied "+url, toastInfo)
	}
}

// card begins a bordered, padded panel; end it with endCard.
func card(id string, w, h float32, flags imgui.ChildFlags) bool {
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cPanel))
	imgui.PushStyleColorVec4(imgui.ColBorder, vec4(cBorder.alpha(0.6)))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(14, 12))
	return imgui.BeginChildStrV(id, v2(w, h), imgui.ChildFlagsBorders|imgui.ChildFlagsAlwaysUseWindowPadding|flags, 0)
}

func endCard() {
	imgui.EndChild()
	imgui.PopStyleVar()
	imgui.PopStyleColorV(2)
}

// section is a heading with a rule, for grouping content in a pane.
func section(glyph, title string) {
	imgui.Spacing()
	imgui.PushStyleColorVec4(imgui.ColText, vec4(cMuted))
	imgui.SeparatorText(glyph + "  " + strings.ToUpper(title))
	imgui.PopStyleColor()
}

// logLines renders output lines in the mono font with a clipper, sticking to
// the bottom while the view is scrolled to the end.
func logLines(id string, lines []string, h float32) {
	imgui.PushStyleColorVec4(imgui.ColChildBg, vec4(cBgDark))
	imgui.PushStyleVarVec2(imgui.StyleVarWindowPadding, v2(10, 8))
	if imgui.BeginChildStrV(id, v2(0, h), imgui.ChildFlagsAlwaysUseWindowPadding, imgui.WindowFlagsHorizontalScrollbar) {
		stick := imgui.ScrollY() >= imgui.ScrollMaxY()-2
		withFont(fontMono, 0, func() {
			if len(lines) == 0 {
				textC(cDim, "No output yet.")
			}
			clip := imgui.NewListClipper()
			clip.Begin(int32(len(lines)))
			for clip.Step() {
				for i := clip.DisplayStart(); i < clip.DisplayEnd(); i++ {
					l := lines[i]
					switch low := strings.ToLower(l); {
					case strings.Contains(low, "error") || strings.Contains(low, "fail"):
						textC(cError, l)
					case strings.Contains(low, "warn"):
						textC(cWarning, l)
					default:
						text(l)
					}
				}
			}
			clip.End()
			clip.Destroy()
		})
		if stick {
			imgui.SetScrollHereYV(1)
		}
	}
	imgui.EndChild()
	imgui.PopStyleVar()
	imgui.PopStyleColor()
}

// rightAlign moves the cursor so an item of width w ends at the region's edge.
func rightAlign(w float32) {
	avail := imgui.ContentRegionAvail().X
	if avail > w {
		imgui.SetCursorPosX(imgui.CursorPosX() + avail - w)
	}
}
