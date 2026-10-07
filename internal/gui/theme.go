//go:build !nogui

package gui

import (
	_ "embed"
	"os"
	"os/exec"
	"strings"

	"github.com/AllenDang/cimgui-go/imgui"
	g "github.com/AllenDang/giu"
)

// faSolid is Font Awesome Free 6 Solid, SIL OFL 1.1 (fonts/LICENSE-fontawesome.txt).
//
//go:embed fonts/fa-solid-900.ttf
var faSolid []byte

const baseFontSize = 16

// Text fonts, first found wins; fc-match is asked when none of these exist.
var (
	textFontPaths = []string{
		"/usr/share/fonts/truetype/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/google-noto/NotoSans-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/TTF/DejaVuSans.ttf",
		"/usr/share/fonts/dejavu-sans-fonts/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/System/Library/Fonts/Supplemental/Arial.ttf",
	}
	boldFontPaths = []string{
		"/usr/share/fonts/truetype/noto/NotoSans-SemiBold.ttf",
		"/usr/share/fonts/truetype/noto/NotoSans-Bold.ttf",
		"/usr/share/fonts/noto/NotoSans-Bold.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/TTF/DejaVuSans-Bold.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Bold.ttf",
	}
	monoFontPaths = []string{
		"/usr/share/fonts/truetype/jetbrains-mono/JetBrainsMono-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf",
		"/usr/share/fonts/TTF/DejaVuSansMono.ttf",
		"/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationMono-Regular.ttf",
		"/System/Library/Fonts/Menlo.ttc",
	}
)

var fontBold, fontMono *g.FontInfo

func readFirst(paths []string, fcPattern string) []byte {
	for _, p := range paths {
		if b, err := os.ReadFile(p); err == nil { // #nosec G304 -- p is from the fixed font path list
			return b
		}
	}
	if fcPattern == "" {
		return nil
	}
	out, err := exec.Command("fc-match", "-f", "%{file}", fcPattern).Output() // #nosec G204 -- fcPattern is a constant font pattern
	if err != nil {
		return nil
	}
	p := strings.TrimSpace(string(out))
	if !strings.HasSuffix(strings.ToLower(p), ".ttf") && !strings.HasSuffix(strings.ToLower(p), ".otf") {
		return nil
	}
	b, _ := os.ReadFile(p) // #nosec G304 -- p is a font file path reported by fc-match
	return b
}

// setupTheme installs the fonts (text font with the icon font merged into it,
// plus bold and mono) and the Tokyo Night style.
func setupTheme(mw *g.MasterWindow) {
	atlas := g.Context.FontAtlas
	atlas.SetDefaultFontSize(baseFontSize)
	// SetDefaultFontFromBytes prepends, and every default font after the first
	// is merged into it: icons go in first so the text font ends up in front.
	atlas.SetDefaultFontFromBytes(faSolid)
	if text := readFirst(textFontPaths, "sans-serif:style=Regular"); text != nil {
		atlas.SetDefaultFontFromBytes(text)
	}
	if b := readFirst(boldFontPaths, "sans-serif:style=Bold"); b != nil {
		fontBold = atlas.AddFontFromBytes("homelab-bold", b)
	}
	if m := readFirst(monoFontPaths, "monospace"); m != nil {
		fontMono = atlas.AddFontFromBytes("homelab-mono", m)
	}

	mw.SetStyle(nil) // giu's theme is pushed every frame; ours lives in the style itself
	bg := hexColor(hexBg)
	mw.SetBgColor(colorOf(bg))

	s := imgui.CurrentStyle()
	s.SetWindowPadding(imgui.Vec2{X: 14, Y: 12})
	s.SetFramePadding(imgui.Vec2{X: 10, Y: 6})
	s.SetItemSpacing(imgui.Vec2{X: 8, Y: 8})
	s.SetItemInnerSpacing(imgui.Vec2{X: 6, Y: 6})
	s.SetCellPadding(imgui.Vec2{X: 8, Y: 6})
	s.SetScrollbarSize(12)
	s.SetGrabMinSize(10)
	s.SetWindowRounding(8)
	s.SetChildRounding(8)
	s.SetFrameRounding(6)
	s.SetPopupRounding(8)
	s.SetScrollbarRounding(6)
	s.SetGrabRounding(6)
	s.SetTabRounding(6)
	s.SetWindowBorderSize(0)
	s.SetChildBorderSize(1)
	s.SetPopupBorderSize(1)
	s.SetFrameBorderSize(0)
	s.SetTabBarBorderSize(1)
	s.SetSeparatorTextBorderSize(1)
	s.SetSeparatorTextPadding(imgui.Vec2{X: 0, Y: 4})
	s.SetSelectableTextAlign(imgui.Vec2{X: 0, Y: 0.5})

	cols := s.Colors()
	set := func(id imgui.Col, c rgba) { cols[id] = vec4(c) }
	text, muted, border := hexColor(hexText), hexColor(hexComment), hexColor(hexBorder)
	primary, accent := hexColor(hexPrimary), hexColor(hexAccent)
	panel, raised, hover, active := hexColor(hexBgPanel), hexColor(hexBgRaised), hexColor(hexBgHover), hexColor(hexBgActive)
	set(imgui.ColText, text)
	set(imgui.ColTextDisabled, hexColor(hexMuted))
	set(imgui.ColWindowBg, bg)
	set(imgui.ColChildBg, rgba{})
	set(imgui.ColPopupBg, panel)
	set(imgui.ColBorder, border.alpha(0.7))
	set(imgui.ColBorderShadow, rgba{})
	set(imgui.ColFrameBg, raised)
	set(imgui.ColFrameBgHovered, hover)
	set(imgui.ColFrameBgActive, active)
	set(imgui.ColTitleBg, panel)
	set(imgui.ColTitleBgActive, panel)
	set(imgui.ColTitleBgCollapsed, panel)
	set(imgui.ColMenuBarBg, panel)
	set(imgui.ColScrollbarBg, rgba{})
	set(imgui.ColScrollbarGrab, border)
	set(imgui.ColScrollbarGrabHovered, muted)
	set(imgui.ColScrollbarGrabActive, primary.alpha(0.7))
	set(imgui.ColCheckMark, primary)
	set(imgui.ColSliderGrab, primary)
	set(imgui.ColSliderGrabActive, accent)
	set(imgui.ColButton, raised)
	set(imgui.ColButtonHovered, hover)
	set(imgui.ColButtonActive, active)
	set(imgui.ColHeader, active.alpha(0.55))
	set(imgui.ColHeaderHovered, hover)
	set(imgui.ColHeaderActive, active)
	set(imgui.ColSeparator, border)
	set(imgui.ColSeparatorHovered, primary.alpha(0.6))
	set(imgui.ColSeparatorActive, primary)
	set(imgui.ColResizeGrip, rgba{})
	set(imgui.ColTab, panel)
	set(imgui.ColTabHovered, hover)
	set(imgui.ColTabSelected, raised)
	set(imgui.ColTabSelectedOverline, primary)
	set(imgui.ColTabDimmed, panel)
	set(imgui.ColTabDimmedSelected, raised)
	set(imgui.ColTableHeaderBg, panel)
	set(imgui.ColTableBorderStrong, border)
	set(imgui.ColTableBorderLight, border.alpha(0.5))
	set(imgui.ColTableRowBg, rgba{})
	set(imgui.ColTableRowBgAlt, raised.alpha(0.35))
	set(imgui.ColTextSelectedBg, primary.alpha(0.3))
	set(imgui.ColNavCursor, primary)
	set(imgui.ColModalWindowDimBg, hexColor(hexBgDark).alpha(0.7))
	s.SetColors(&cols)

	mw.SetScale(0) // DPI: scale the sizes set above and the font
}

func vec4(c rgba) imgui.Vec4 { return imgui.Vec4{X: c.R, Y: c.G, Z: c.B, W: c.A} }
