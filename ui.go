package main

// ui.go — the Kouki main window.
//
// Why the whole window is one drawn canvas
// ----------------------------------------
// walk is a thin binding over the Win32 common controls. Two of its properties
// make a styled UI impossible through the stock widgets:
//
//  1. No text colour. WidgetBase exposes no SetForegroundColor, so a label on a
//     dark panel stays black and a Button/CheckBox keeps the system colours —
//     white buttons over a dark background, which is the look being replaced.
//  2. CustomWidget has no size of its own. Its layout item is a hard-coded
//     greedyLayoutItem with IdealSize 100x100 and MinSize 50x50 (walk/layout.go),
//     which overrides SetMinMaxSize entirely. Every drawn control would be at
//     least 50 dialog units tall and several would fight each other for the
//     leftover space.
//
// Both problems disappear when the window is a single walk.CustomWidget that
// paints everything: colours, corner radii, control heights and spacing become
// plain arithmetic on pixels, while the result is still a real native window
// with a real message loop, the tray, and the dialogs.
//
// What walk still provides, and what is used here
// ------------------------------------------------
//   - the window, its message loop and close/minimise-to-tray
//   - MainWindow.Synchronize for cross-thread repaints
//   - CustomWidget's PaintFunc as the drawing surface
//   - MouseDown/MouseUp/MouseMove/MouseWheel and KeyDown for input
//   - the native tooltip, whose text follows the control under the cursor
//   - guiAction, the tray menu and every dialog, all unchanged
//
// What this approach cannot do
// ----------------------------
//   - text selection or a caret in the read-only lines
//   - accessibility (UI Automation) metadata for the drawn controls
//   - follow the high-contrast themes, whose palette would replace the one below
//   - resize itself to the content: the window is fixed-size and the content
//     scrolls, which keeps the layout identical at every window size
//
// The layout is rebuilt from live state on every paint, so there is no separate
// synchronisation path: if the store, the player or the config changed, the next
// repaint already shows it.

import (
	"bytes"
	"image"
	"image/png"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------------------
// palette
// ---------------------------------------------------------------------------

// One accent, one background family, three text weights, three status colours.
// Nothing here is decorative.
var (
	cBackground = walk.RGB(0x11, 0x13, 0x18) // #111318 window background
	cSurface    = walk.RGB(0x19, 0x1D, 0x26) // #191D26 cards
	cSurfaceAlt = walk.RGB(0x20, 0x26, 0x33) // #202633 raised surfaces
	cBorder     = walk.RGB(0x30, 0x38, 0x47) // #303847 card hairlines
	cBorderSoft = walk.RGB(0x27, 0x2E, 0x3A) // seams inside a card
	cAccent     = walk.RGB(0x45, 0xB8, 0xF0) // #45B8F0 accent
	cAccentHov  = walk.RGB(0x63, 0xC7, 0xF7)
	cAccentDown = walk.RGB(0x33, 0x9A, 0xCC)

	cText      = walk.RGB(0xE8, 0xED, 0xF5) // #E8EDF5 primary text
	cTextMuted = walk.RGB(0xA0, 0xAA, 0xBD) // #A0AABD secondary text
	cTextDim   = walk.RGB(0x66, 0x70, 0x84) // tertiary, and disabled

	cWhite = walk.RGB(0xFF, 0xFF, 0xFF)

	cOK   = walk.RGB(0x4A, 0xDE, 0x80)
	cWarn = walk.RGB(0xFB, 0xBF, 0x24)
	cErr  = walk.RGB(0xF8, 0x71, 0x71)
)

// ---------------------------------------------------------------------------
// metrics, in physical pixels
// ---------------------------------------------------------------------------

// Metrics, in native pixels at the running display's DPI.
//
// They used to be compile-time constants in 96 dpi units while the fonts were
// scaled by DPI. That mismatch is what clipped every descender and every glyph:
// above 100% scaling the font's line box grew taller than the fixed rect it was
// drawn into, and DrawTextEx — which clips to its rect — cut the difference off.
// "progresso" rendered as "proaresso", "Flugel" as "Fluqel", and the card icons
// looked swallowed. Fonts and boxes now move together, so the ratio holds at
// every DPI.
//
// applyMetrics is the single place that recomputes them, called from
// loadUIFonts once the DPI is known.
var (
	mPad     int // window margin
	mColGap  int // gap between the two columns
	mGap     int // gap between rows of cards
	mCardPad int // card padding

	mHeaderH int // header strip
	mLogo    int

	mTitleH int // card title line
	mSubH   int // card subtitle line

	mInnerGap int // gap between rows inside a card
	mSepH     int // hairline between two groups of rows in a card

	// mMinColW is the narrowest a column may get before the layout gives up on
	// two of them. Below it the buttons and the switch labels start to clip.
	mMinColW int

	mBtnH    int // primary and secondary actions
	mToolH   int // quieter action rows
	mSwitchH int // toggle rows
	mTextH   int // read-only lines
)

// uiScale is the factor the 96 dpi design is multiplied by.
var uiScale = 1.0

// uiMetric scales one 96 dpi measurement to the running display. It is
// standalone so loadUIFonts can reuse it while it builds the remaining values.
func uiMetric(v int) int {
	n := int(float64(v)*uiScale + 0.5)
	if n < 1 {
		n = 1
	}
	return n
}

// applyMetrics recomputes every measurement for a display scale.
func applyMetrics(scale float64) {
	if scale <= 0 {
		scale = 1
	}
	uiScale = scale

	mPad, mColGap, mGap, mCardPad = uiMetric(22), uiMetric(14), uiMetric(14), uiMetric(18)
	mHeaderH, mLogo = uiMetric(50), uiMetric(36)
	mTitleH, mSubH = uiMetric(20), uiMetric(16)
	mInnerGap, mSepH = uiMetric(10), uiMetric(1)
	mMinColW = uiMetric(340)
	mBtnH, mToolH, mSwitchH, mTextH = uiMetric(32), uiMetric(29), uiMetric(32), uiMetric(17)
}

// ---------------------------------------------------------------------------
// glyphs
// ---------------------------------------------------------------------------

// Segoe MDL2 Assets ships with Windows 10 and later, so these are real
// interface icons with no image dependency and no emoji.
const (
	iconPlay    = "\uE768"
	iconPause   = "\uE769"
	iconStop    = "\uE71A"
	iconSearch  = "\uE721"
	iconFolder  = "\uE8B7"
	iconGear    = "\uE713"
	iconInfo    = "\uE946"
	iconSync    = "\uE895"
	iconPower   = "\uE7A7"
	iconCheck   = "\uE73E"
	iconAccount = "\uE77B"
)

// ---------------------------------------------------------------------------
// fonts
// ---------------------------------------------------------------------------

// walk.NewFont builds an HFONT at a physical point size and, unlike the rest of
// the toolkit, has no DPI scaling hook. The sizes below are therefore logical
// and get scaled once, here, so the window keeps its proportions at 125%, 150%
// or 175%.
var (
	uiFontsLoaded bool
	uiHasIcons    bool

	fAppName *walk.Font
	fTagline *walk.Font
	fTitle   *walk.Font
	fSub     *walk.Font
	fBody    *walk.Font
	fMuted   *walk.Font
	fTiny    *walk.Font
	fButton  *walk.Font
	fLogo    *walk.Font
	fIcon    *walk.Font
)

// Icon slots, the status pill and the meta column also move with the display.
var (
	mIconTitle  int // card-title glyph box
	mIconState  int // state glyph box inside the header pill
	statusPillW int // fits the longest caption, "Desconectado"
	metaWidth   int // reserved slot for a card's right-aligned value
)

// fontRegular is the zero FontStyle. walk exports FontBold, FontItalic,
// FontUnderline and FontStrikeOut, but no FontNormal: a plain face is simply the
// absence of a flag.
const fontRegular = walk.FontStyle(0)

func loadUIFonts(dpi int) {
	if uiFontsLoaded {
		return
	}
	if dpi <= 0 {
		dpi = 96
	}

	// Logical sizes are tuned for 96 dpi.
	scale := func(pt int) int {
		v := pt * dpi / 96
		if v < 7 {
			v = 7
		}
		return v
	}

	// Boxes first: everything below is expressed in the same pixels.
	applyMetrics(float64(dpi) / 96.0)

	mk := func(family string, pt int, style walk.FontStyle) *walk.Font {
		f, err := walk.NewFont(family, scale(pt), style)
		if err != nil {
			return nil
		}
		return f
	}

	const (
		segoeUI = "Segoe UI"
		mdl2    = "Segoe MDL2 Assets"
	)

	fLogo = mk(segoeUI, 15, walk.FontBold)
	fAppName = mk(segoeUI, 16, walk.FontBold)

	fTagline = mk(segoeUI, 9, fontRegular)
	fTitle = mk(segoeUI, 11, walk.FontBold)
	fSub = mk(segoeUI, 8, fontRegular)
	fBody = mk(segoeUI, 9, fontRegular)
	fMuted = mk(segoeUI, 9, fontRegular)
	fTiny = mk(segoeUI, 8, fontRegular)
	fButton = mk(segoeUI, 9, walk.FontBold)

	// The MDL2 glyph face needs to be noticeably larger than the text faces: the
	// glyph box is square and sits on the em, so a size that looks right next to
	// 9 pt text renders at roughly 13 px of ink and reads as a speck next to an
	// 18-20 px title.
	fIcon = mk(mdl2, 11, fontRegular)

	// Icon slots are pixel widths, so they have to grow with the glyphs or the
	// marks get clipped at their own box.
	mIconTitle = uiMetric(18)
	mIconState = uiMetric(16)

	// The status pill must fit its longest caption, "Desconectado", at whatever
	// DPI the font was scaled to. A fixed pixel width truncated the text on a
	// 125% or 150% display while leaving it loose at 100%.
	statusPillW = uiMetric(196)
	metaWidth = uiMetric(148)

	// A machine without Segoe MDL2 Assets simply draws no icons. Every label
	// already carries its own text, so nothing is lost.
	uiHasIcons = fIcon != nil

	// Whatever happens above, fBody must exist: a nil font inside DrawTextEx is
	// not survivable.
	if fBody == nil {
		if f, err := walk.NewFont(segoeUI, 9, fontRegular); err == nil {
			fBody = f
		}
	}
	if fBody == nil {
		if f, err := walk.NewFont("", 9, fontRegular); err == nil {
			fBody = f
		}
	}
	// Only a face that could not be built falls back to the body face. These
	// three lines used to assign fBody to all of them unconditionally, which threw
	// away every size and weight built above: the wordmark, the card titles and
	// the button labels all rendered as 9 pt regular, and the hierarchy the layout
	// was designed around did not exist on screen.
	for _, f := range []**walk.Font{&fMuted, &fTagline, &fTiny, &fSub, &fButton, &fTitle, &fAppName, &fLogo} {
		if *f == nil {
			*f = fBody
		}
	}

	uiFontsLoaded = true
}

// uiFont never returns nil, so painting cannot hand DrawTextEx a nil face.
func uiFont(f *walk.Font) *walk.Font {
	if f != nil {
		return f
	}
	if fBody != nil {
		return fBody
	}
	loadUIFonts(96)
	return fBody
}

// ---------------------------------------------------------------------------
// brushes
// ---------------------------------------------------------------------------

// Brushes are GDI objects and must not be rebuilt per paint. Only walk's thread
// paints, so this map needs no lock.
var uiBrushes = map[walk.Color]walk.Brush{}

func uiBrush(c walk.Color) walk.Brush {
	if b, ok := uiBrushes[c]; ok {
		return b
	}
	b, err := walk.NewSolidColorBrush(c)
	if err != nil {
		return walk.NullBrush()
	}
	uiBrushes[c] = b
	return b
}

// ---------------------------------------------------------------------------
// geometry and colour helpers
// ---------------------------------------------------------------------------

func uiInset(r walk.Rectangle, n int) walk.Rectangle {
	return walk.Rectangle{X: r.X + n, Y: r.Y + n, Width: r.Width - 2*n, Height: r.Height - 2*n}
}

// uiPadX shrinks a rectangle horizontally only. A label inside a
// vertically-centred control needs side padding without losing the height that
// centres it.
func uiPadX(r walk.Rectangle, dx int) walk.Rectangle {
	return walk.Rectangle{X: r.X + dx, Y: r.Y, Width: r.Width - 2*dx, Height: r.Height}
}

func uiExpand(r walk.Rectangle, n int) walk.Rectangle {
	return walk.Rectangle{X: r.X - n, Y: r.Y - n, Width: r.Width + 2*n, Height: r.Height + 2*n}
}

// uiMix blends b over a: t = 0 keeps a, t = 1 gives b.
func uiMix(a, b walk.Color, t float64) walk.Color {
	switch {
	case t <= 0:
		return a
	case t >= 1:
		return b
	}
	mix := func(x, y byte) byte {
		return byte(float64(x) + (float64(y)-float64(x))*t + 0.5)
	}
	return walk.RGB(mix(a.R(), b.R()), mix(a.G(), b.G()), mix(a.B(), b.B()))
}

// uiTint lays a colour over the card background. This is how the status washes
// and the focus halo are produced without a second surface brush.
func uiTint(c walk.Color, t float64) walk.Color { return uiMix(cSurface, c, t) }

func uiMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func uiMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// drawing primitives
// ---------------------------------------------------------------------------

// uiFill paints a rounded rectangle; radius 0 draws a plain box.
//
// Rounded fills go through uiRoundFill, which antialiases the corners. GDI's
// RoundRect rasterises with a one-bit mask, so every card, button and pill had
// stair-stepped corners, and it takes the corner *diameter*, so the radii passed
// here were silently halved as well.
func uiFill(c *walk.Canvas, r walk.Rectangle, radius int, col walk.Color) {
	if r.Width <= 0 || r.Height <= 0 {
		return
	}
	if radius <= 0 {
		_ = c.FillRectanglePixels(uiBrush(col), r)
		return
	}
	uiRoundFill(c, r, radius, col)
}

// uiPanel fills a rounded rect with a one-pixel border. Canvas exposes no
// stroke, so the border is a slightly larger rounded rect painted underneath.
func uiPanel(c *walk.Canvas, r walk.Rectangle, radius int, fill, border walk.Color) {
	uiFill(c, r, radius, border)
	uiFill(c, uiInset(r, 1), uiMaxInt(0, radius-1), fill)
}

// uiText writes one line. The alignment flags are DrawTextFormat values, so the
// vertical centring and the single-line truncation are always applied.
//
// The rect is grown vertically before drawing. DrawTextEx clips to the rect it is
// given, and a rect sized to the label alone cuts the ascenders and descenders of
// the face — which is what turned "progresso" into "proaresso" and "Flugel" into
// "Fluqel". Growing by the face's own line height and centring in the result means
// the whole glyph box is inside the rect at every DPI, with no cost beyond a few
// pixels of slack.
func uiText(c *walk.Canvas, s string, font *walk.Font, col walk.Color, r walk.Rectangle, align walk.DrawTextFormat) {
	f := uiFont(font)
	if f == nil || s == "" || r.Width <= 0 || r.Height <= 0 {
		return
	}

	h := uiLineHeight(c, f)
	if r.Height < h {
		grow := h - r.Height
		r.Y -= grow / 2
		r.Height = h
	}

	_ = c.DrawTextPixels(s, f, col, r,
		align|walk.TextVCenter|walk.TextSingleLine|walk.TextEndEllipsis|walk.TextNoPrefix)
}

// uiLineHeight returns the full line box of a face, so text rows can be sized
// from the font instead of from a guess that only holds at 96 dpi.
//
// walk keeps its own fontHeight private, but MeasureTextPixels is exported and
// reports the same box. The probe string carries a descender and an accent
// because "Ag" is what the metrics are actually about: it has to survive, so it
// has to fit.
func uiLineHeight(c *walk.Canvas, f *walk.Font) int {
	// Every text draw asks for this, so measuring afresh meant a GDI measurement
	// per label per paint. A face's line box never changes. Walk's thread only.
	if h, ok := uiLineHeights[f]; ok {
		return h
	}
	h := uiMeasureLineHeight(c, f)
	uiLineHeights[f] = h
	return h
}

var uiLineHeights = map[*walk.Font]int{}

func uiMeasureLineHeight(c *walk.Canvas, f *walk.Font) int {
	bounds, _, err := c.MeasureTextPixels("AgjyQ", f, walk.Rectangle{Width: uiMetric(400), Height: uiMetric(200)},
		walk.TextSingleLine|walk.TextNoPrefix)
	h := 0
	if err == nil {
		h = bounds.Height
	}
	if h <= 0 {
		h = uiMetric(18)
	}
	// A little slack for ink that sits just outside the nominal box, and never
	// less than the row itself.
	if min := uiMetric(16); h < min {
		h = min
	}
	return h
}

func uiTextLeft(c *walk.Canvas, s string, font *walk.Font, col walk.Color, r walk.Rectangle) {
	uiText(c, s, font, col, r, walk.TextLeft)
}

func uiTextCenter(c *walk.Canvas, s string, font *walk.Font, col walk.Color, r walk.Rectangle) {
	uiText(c, s, font, col, r, walk.TextCenter)
}

func uiTextRight(c *walk.Canvas, s string, font *walk.Font, col walk.Color, r walk.Rectangle) {
	uiText(c, s, font, col, r, walk.TextRight)
}

// ---------------------------------------------------------------------------
// antialiased shapes
// ---------------------------------------------------------------------------

// GDI has no antialiased primitive. FillRoundedRectanglePixels is a bare
// win.RoundRect and FillEllipsePixels a bare win.Ellipse, and both rasterise with
// a one-bit mask: each pixel is painted or it is not, and every curve comes out
// as a staircase. A switch is the worst case in the window for it — a 40x22
// stadium with 11 px caps carrying a 16 px circle, in white on #45B8F0 over
// #191D26, the highest contrast pair the palette contains. The eye reads the
// steps as dirt on the shape rather than as an edge.
//
// So the curved shapes stop being asked of GDI. Each one is rasterised once
// into a 32-bit alpha bitmap by evaluating the exact distance of every pixel to
// the outline, and walk blits it through win.AlphaBlend — which is antialiased.
// Coverage comes from that distance directly: a pixel whose centre sits half a
// pixel outside the edge is half painted, which is a smooth ramp over 256
// levels instead of a 2-level mask.

// uiRoundRectDist is the signed distance from a point to a rounded rectangle:
// negative inside, positive outside, magnitude in pixels.
//
// The distance form is what makes the coverage smooth. Testing the four corner
// quadrants separately, as GDI does, puts a hard step at each one.
func uiRoundRectDist(px, py, w, h, radius float64) float64 {
	hw, hh := w/2, h/2
	r := radius
	if r > hw {
		r = hw
	}
	if r > hh {
		r = hh
	}
	if r < 0 {
		r = 0
	}
	// Offset from the straight edges, in each axis, of the corner-free core.
	qx := math.Abs(px-hw) - (hw - r)
	qy := math.Abs(py-hh) - (hh - r)
	mx, my := math.Max(qx, 0), math.Max(qy, 0)
	return math.Sqrt(mx*mx+my*my) + math.Min(math.Max(qx, qy), 0) - r
}

// uiAShape is one rasterised shape, kept alive across repaints. Rebuilding it
// means a CreateDIBSection and another GDI object, which is not work the repaint
// path should be doing every frame.
type uiAShape struct {
	bmp *walk.Bitmap
}

// uiAShapes caches rasterised shapes by geometry and colour. There are only a
// handful: the track and the thumb in their few states plus the focus ring, all
// at fixed sizes, so the cache settles after the first paint and stays small.
var (
	uiAShapesMu sync.Mutex
	uiAShapes   = map[string]*uiAShape{}
)

// uiRoundShape returns the cached bitmap for a rounded rectangle of w by h with
// the given corner radius, rasterising it on first use. radius of half the
// shorter side gives a stadium; equal to half of both gives a circle.
func uiRoundShape(w, h, radius int, col walk.Color) *uiAShape {
	if w <= 0 || h <= 0 {
		return nil
	}

	key := strconv.Itoa(w) + "|" + strconv.Itoa(h) + "|" + strconv.Itoa(radius) + "|" +
		strconv.Itoa(int(col.R())) + "." + strconv.Itoa(int(col.G())) + "." + strconv.Itoa(int(col.B()))

	uiAShapesMu.Lock()
	defer uiAShapesMu.Unlock()
	if s, ok := uiAShapes[key]; ok {
		return s
	}
	// Bounded: a window being resized produces a new size on every step, and each
	// entry holds a GDI bitmap.
	if len(uiAShapes) > 128 {
		for _, old := range uiAShapes {
			if old != nil && old.bmp != nil {
				old.bmp.Dispose()
			}
		}
		uiAShapes = map[string]*uiAShape{}
	}

	im := image.NewRGBA(image.Rect(0, 0, w, h))
	r8, g8, b8 := col.R(), col.G(), col.B()
	fw, fh, fr := float64(w), float64(h), float64(radius)

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			cover := 0.5 - uiRoundRectDist(float64(x)+0.5, float64(y)+0.5, fw, fh, fr)
			if cover <= 0 {
				continue
			}
			if cover > 1 {
				cover = 1
			}
			a := uint16(cover*255 + 0.5)
			off := im.PixOffset(x, y)
			// image.RGBA is alpha-premultiplied. walk's hBitmapFromImage copies
			// all four channels into a 32-bit BI_BITFIELDS DIB whose alpha mask
			// is the high byte, and Bitmap.draw blits it through AlphaBlend, so
			// the coverage above arrives at the screen intact.
			im.Pix[off+0] = uint8((uint16(r8)*a + 127) / 255)
			im.Pix[off+1] = uint8((uint16(g8)*a + 127) / 255)
			im.Pix[off+2] = uint8((uint16(b8)*a + 127) / 255)
			im.Pix[off+3] = uint8(a)
		}
	}

	bmp, err := walk.NewBitmapFromImage(im)
	if err != nil {
		// A missing shape is better drawn aliased than not drawn at all.
		return nil
	}
	s := &uiAShape{bmp: bmp}
	uiAShapes[key] = s
	return s
}

// uiRoundFill fills a rounded rectangle with antialiased edges. A radius of zero
// is a plain box and still goes through GDI, which has nothing wrong with a
// straight edge.
func uiRoundFill(c *walk.Canvas, r walk.Rectangle, radius int, col walk.Color) {
	if r.Width <= 0 || r.Height <= 0 {
		return
	}
	rad := uiMinInt(radius, uiMinInt(r.Width/2, r.Height/2))
	if rad <= 0 {
		_ = c.FillRectanglePixels(uiBrush(col), r)
		return
	}

	// Small shapes (the switch, its thumb, dots) are one bitmap. Their sizes are
	// fixed, so the cache settles after the first paint.
	if r.Width <= 64 && r.Height <= 64 {
		s := uiRoundShape(r.Width, r.Height, rad, col)
		if s == nil {
			_ = c.FillRoundedRectanglePixels(uiBrush(col), r, walk.Size{Width: 2 * rad, Height: 2 * rad})
			return
		}
		_ = c.DrawImagePixels(s.bmp, walk.Point{X: r.X, Y: r.Y})
		return
	}

	// Cards, buttons and the status pill change width with the window. A bitmap
	// per size would pile up while the window is dragged, so large shapes are
	// three opaque rectangles plus four small corner bitmaps whose cache key is
	// only the radius and the colour.
	cs := uiCorners(rad, col)
	if cs == nil {
		_ = c.FillRoundedRectanglePixels(uiBrush(col), r, walk.Size{Width: 2 * rad, Height: 2 * rad})
		return
	}
	box := func(x, y, w, h int) {
		if w > 0 && h > 0 {
			_ = c.FillRectanglePixels(uiBrush(col), walk.Rectangle{X: x, Y: y, Width: w, Height: h})
		}
	}
	box(r.X+rad, r.Y, r.Width-2*rad, r.Height)
	box(r.X, r.Y+rad, rad, r.Height-2*rad)
	box(r.X+r.Width-rad, r.Y+rad, rad, r.Height-2*rad)
	_ = c.DrawImagePixels(cs.tl, walk.Point{X: r.X, Y: r.Y})
	_ = c.DrawImagePixels(cs.tr, walk.Point{X: r.X + r.Width - rad, Y: r.Y})
	_ = c.DrawImagePixels(cs.bl, walk.Point{X: r.X, Y: r.Y + r.Height - rad})
	_ = c.DrawImagePixels(cs.br, walk.Point{X: r.X + r.Width - rad, Y: r.Y + r.Height - rad})
}

// uiCornerSet is the four quadrants of an antialiased circle.
type uiCornerSet struct{ tl, tr, bl, br *walk.Bitmap }

var uiCornerCache = map[string]*uiCornerSet{}

// uiCorners rasterises the corners for one radius and colour. Each quadrant is
// cut out of a circle of diameter 2*radius with the same exact-distance coverage
// the whole-shape path uses, so both look identical.
func uiCorners(radius int, col walk.Color) *uiCornerSet {
	if radius <= 0 {
		return nil
	}
	key := strconv.Itoa(radius) + "|" +
		strconv.Itoa(int(col.R())) + "." + strconv.Itoa(int(col.G())) + "." + strconv.Itoa(int(col.B()))

	uiAShapesMu.Lock()
	defer uiAShapesMu.Unlock()
	if s, ok := uiCornerCache[key]; ok {
		return s
	}
	if len(uiCornerCache) > 256 {
		for _, old := range uiCornerCache {
			for _, b := range []*walk.Bitmap{old.tl, old.tr, old.bl, old.br} {
				if b != nil {
					b.Dispose()
				}
			}
		}
		uiCornerCache = map[string]*uiCornerSet{}
	}

	r8, g8, b8 := col.R(), col.G(), col.B()
	diameter := float64(2 * radius)
	build := func(ox, oy int) *walk.Bitmap {
		im := image.NewRGBA(image.Rect(0, 0, radius, radius))
		for y := 0; y < radius; y++ {
			for x := 0; x < radius; x++ {
				cover := 0.5 - uiRoundRectDist(float64(x+ox)+0.5, float64(y+oy)+0.5, diameter, diameter, float64(radius))
				if cover <= 0 {
					continue
				}
				if cover > 1 {
					cover = 1
				}
				a := uint16(cover*255 + 0.5)
				off := im.PixOffset(x, y)
				// Premultiplied, as in uiRoundShape.
				im.Pix[off+0] = uint8((uint16(r8)*a + 127) / 255)
				im.Pix[off+1] = uint8((uint16(g8)*a + 127) / 255)
				im.Pix[off+2] = uint8((uint16(b8)*a + 127) / 255)
				im.Pix[off+3] = uint8(a)
			}
		}
		bmp, err := walk.NewBitmapFromImage(im)
		if err != nil {
			return nil
		}
		return bmp
	}

	set := &uiCornerSet{tl: build(0, 0), tr: build(radius, 0), bl: build(0, radius), br: build(radius, radius)}
	if set.tl == nil || set.tr == nil || set.bl == nil || set.br == nil {
		return nil // the caller draws it aliased rather than not at all
	}
	uiCornerCache[key] = set
	return set
}

// uiRoundDot fills a circle with antialiased edges.
func uiRoundDot(c *walk.Canvas, x, y, d int, col walk.Color) {
	uiRoundFill(c, walk.Rectangle{X: x, Y: y, Width: d, Height: d}, d/2, col)
}

// ---------------------------------------------------------------------------
// application icon
// ---------------------------------------------------------------------------

// The header shows the real artwork instead of the letter "K" in a coloured
// square. The 512 px master is resampled down with an area average, which is the
// correct filter for a reduction this large: every destination pixel takes the
// mean of all the source pixels inside it, so a 14:1 shrink stays smooth
// instead of aliasing into noise the way nearest-neighbour does, and without the
// dither pattern GDI's HALFTONE stretch would lay over it.

// appIconSource decodes the embedded master exactly once.
var (
	appIconOnce sync.Once
	appIconImg  image.Image
)

func appIconSource() image.Image {
	appIconOnce.Do(func() {
		b, err := appIconPNG.ReadFile("assets/icon/icon-512.png")
		if err != nil {
			logAppend("icone: leitura falhou: ", err.Error())
			return
		}
		im, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			logAppend("icone: decodificacao falhou: ", err.Error())
			return
		}
		appIconImg = im
	})
	return appIconImg
}

// uiScaleImageTo resamples src to w by h with an area average and returns a
// premultiplied RGBA.
//
// Premultiplied matters: walk's bitmap loader copies all four channels straight
// out of the image into a 32-bit DIB that AlphaBlend reads as-is, so leaving the
// colour un-weighted by coverage would paint a dark fringe around the mark's
// antialiased outline.
func uiScaleImageTo(src image.Image, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	if src == nil || w <= 0 || h <= 0 {
		return dst
	}
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return dst
	}

	for dy := 0; dy < h; dy++ {
		y0, y1 := dy*sh/h, (dy+1)*sh/h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for dx := 0; dx < w; dx++ {
			x0, x1 := dx*sw/w, (dx+1)*sw/w
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var sr, sg, sb, sa, n uint64
			for sy := y0; sy < y1 && sy < sh; sy++ {
				row := b.Min.Y + sy
				for sx := x0; sx < x1 && sx < sw; sx++ {
					// RGBA() is 16-bit and premultiplied, so summing it is already
					// colour weighted by coverage - one pass, no separate alpha.
					r, g, bl, a := src.At(b.Min.X+sx, row).RGBA()
					sr += uint64(r)
					sg += uint64(g)
					sb += uint64(bl)
					sa += uint64(a)
					n++
				}
			}
			if n == 0 {
				continue
			}
			o := dst.PixOffset(dx, dy)
			dst.Pix[o+0] = uint8(sr / n >> 8)
			dst.Pix[o+1] = uint8(sg / n >> 8)
			dst.Pix[o+2] = uint8(sb / n >> 8)
			dst.Pix[o+3] = uint8(sa / n >> 8)
		}
	}
	return dst
}

// uiLogos caches the resampled mark per requested size. There is one size in
// practice, so it settles on the first paint; the key is here because mLogo
// moves with the display.
//
// The bitmaps are held for the life of the process on purpose: rebuilding one
// means a CreateDIBSection and another GDI object, which has no business
// happening on the repaint path.
var (
	uiLogoMu  sync.Mutex
	uiLogos   = map[int]*walk.Bitmap{}
	uiLogoBad bool
)

// uiLogo returns the application mark at size, or nil when the artwork is
// unusable. The caller draws its own fallback.
func uiLogo(size int) *walk.Bitmap {
	if size <= 0 {
		return nil
	}
	uiLogoMu.Lock()
	defer uiLogoMu.Unlock()
	if b, ok := uiLogos[size]; ok {
		return b
	}
	if uiLogoBad || appIconSource() == nil {
		// A decode failure will not fix itself on the next frame, and retrying
		// would re-read the embedded file on every repaint for nothing.
		uiLogoBad = true
		return nil
	}
	bmp, err := walk.NewBitmapFromImage(uiScaleImageTo(appIconSource(), size, size))
	if err != nil {
		uiLogoBad = true
		return nil
	}
	uiLogos[size] = bmp
	return bmp
}

// uiWindowIcon returns the artwork as a walk icon for the title bar, from the
// same master the header paints so the two cannot drift apart.
func uiWindowIcon() walk.Image {
	src := appIconSource()
	if src == nil {
		return nil
	}
	// A fixed master size, deliberately not uiMetric: walk derives every DPI
	// variant from whatever it is handed, so this is called before the display
	// metrics exist and 256 px at 96 dpi already covers a 256 px icon.
	bmp, err := walk.NewBitmapFromImage(uiScaleImageTo(src, 256, 256))
	if err != nil {
		return nil
	}
	ic, err := walk.NewIconFromBitmap(bmp)
	if err != nil {
		return nil
	}
	return ic
}

// uiIcon writes a glyph in the left margin of r and returns the space left over.
//
// w is the glyph's reserved slot and must be at least the face's line box, or
// uiText clips the bottom off the mark. The gap that follows it is scaled too,
// so the title never crowds the glyph as the display grows.
func uiIcon(c *walk.Canvas, glyph string, col walk.Color, r walk.Rectangle, w int) walk.Rectangle {
	if glyph == "" || !uiHasIcons {
		return r
	}
	slot := walk.Rectangle{X: r.X, Y: r.Y, Width: w, Height: r.Height}
	uiTextLeft(c, glyph, fIcon, col, slot)
	gap := w + uiMetric(5)
	r.X += gap
	r.Width -= gap
	return r
}

// ---------------------------------------------------------------------------
// layout model
// ---------------------------------------------------------------------------

// item kinds
const (
	itButton = iota
	itSwitch
	itCaption
	itDivider
)

// Button variants.
//
// bvSecondary is deliberately the zero value: it is what a uiButtonSpec without
// an explicit variant gets, and it is by far the most common. Having bvPrimary
// as zero meant every spec that did not name a variant silently rendered as the
// accent colour, which is why all three playback buttons looked identical.
const (
	bvSecondary = iota
	bvPrimary
	bvGhost
	bvDanger
)

// caption roles
const (
	capBody = iota
	capStrong
	capMuted
	capTiny
)

// uiItem is one drawn element. The slice is rebuilt on every paint; painting and
// hit-testing both read the same one, so they cannot disagree.
type uiItem struct {
	kind    int
	rect    walk.Rectangle
	label   string
	icon    string
	tip     string
	action  string // guiAction name, buttons only
	key     string // setting key, switches only
	role    int    // caption role
	variant int
	right   bool // right-align the caption instead of left-aligning
	state   walk.Color
	enabled bool
	checked bool
}

// uiCard is a panel: rounded surface, icon, title and optional subtitle. Its
// items live in the canvas-wide slice.
type uiCard struct {
	rect     walk.Rectangle
	icon     string
	title    string
	subtitle string
}

type uiButtonSpec struct {
	label   string
	action  string
	variant int // bvSecondary when unset; see the constant block for why
	weight  int // relative width inside the row
	tip     string
}

// uiBuilder lays out one card. Every row consumes its height plus mInnerGap,
// and finish reports the card's total height so the caller can align a pair of
// cards side by side.
type uiBuilder struct {
	u     *uiCanvas
	x, y  int
	w     int
	items []uiItem
	h     int
	first bool
}

func (u *uiCanvas) newCard(x, y, w int, icon, title, subtitle string) *uiBuilder {
	b := &uiBuilder{
		u:     u,
		x:     x,
		y:     y,
		w:     w,
		items: make([]uiItem, 0, 6),
		h:     mCardPad + mTitleH,
		first: true,
	}
	if subtitle != "" {
		b.h += mSubH + 3
	}
	b.h += mInnerGap

	u.cards = append(u.cards, uiCard{
		rect:     walk.Rectangle{X: x, Y: y, Width: w},
		icon:     icon,
		title:    title,
		subtitle: subtitle,
	})
	return b
}

func (b *uiBuilder) rowStart(h int) int {
	if !b.first {
		b.h += mInnerGap
	}
	b.first = false
	top := b.y + b.h
	b.h += h
	return top
}

// buttons lays out one row of buttons. Weights share the width, so a primary
// action can read as clearly more important than the ones beside it.
// uiActionEnabled says whether an action can do anything right now. A button that
// answers every press with an apology dialog ("MPV não está em execução",
// "Nenhum arquivo em reprodução") is worse than one that visibly cannot be
// pressed.
func uiActionEnabled(action string) bool {
	switch action {
	case "mpv-launch":
		return !sharedMpvConn.Connected() && !guiPlayer.IsRunning()
	case "mpv-toggle-pause":
		return sharedMpvConn.Connected()
	case "mpv-stop":
		return sharedMpvConn.Connected() || guiPlayer.IsRunning()
	case "ani-logout":
		return storeAccount("anilist") != ""
	case "mal-logout":
		return storeAccount("mal") != ""
	case "ani-correct", "mal-correct":
		curTrack.mu.RLock()
		defer curTrack.mu.RUnlock()
		return curTrack.Active && curTrack.LastFileKey != ""
	case "simul-host", "simul-join":
		return !uiSimulActive()
	case "simul-stop":
		return uiSimulActive()
	}
	return true
}

func uiSimulActive() bool {
	simul.mu.Lock()
	defer simul.mu.Unlock()
	return simul.mode != simulModeOff
}

// uiGatedActions are the actions whose availability is part of the repaint
// signature, so a change in it repaints the window.
var uiGatedActions = [...]string{
	"mpv-launch", "mpv-toggle-pause", "mpv-stop",
	"ani-logout", "mal-logout", "ani-correct", "mal-correct",
	"simul-host", "simul-join", "simul-stop",
}

func (b *uiBuilder) buttons(h int, specs ...uiButtonSpec) {
	if len(specs) == 0 {
		return
	}
	top := b.rowStart(h)

	inner := b.w - 2*mCardPad
	total := 0
	for _, s := range specs {
		w := s.weight
		if w <= 0 {
			w = 1
		}
		total += w
	}

	gap := uiMetric(8)
	avail := inner - gap*(len(specs)-1)
	if avail < len(specs)*8 {
		avail = len(specs) * 8
	}

	x := b.x + mCardPad
	for _, s := range specs {
		wt := s.weight
		if wt <= 0 {
			wt = 1
		}
		bw := uiMaxInt(8, avail*wt/total)
		b.items = append(b.items, uiItem{
			kind:    itButton,
			rect:    walk.Rectangle{X: x, Y: top, Width: bw, Height: h},
			label:   s.label,
			tip:     s.tip,
			action:  s.action,
			variant: s.variant,
			enabled: uiActionEnabled(s.action),
		})
		x += bw + gap
	}
}

// toggle lays out a switch row bound to a persisted boolean setting.
func (b *uiBuilder) toggle(key, label, tip string, on bool) {
	top := b.rowStart(mSwitchH)
	b.items = append(b.items, uiItem{
		kind:    itSwitch,
		rect:    walk.Rectangle{X: b.x + mCardPad, Y: top, Width: b.w - 2*mCardPad, Height: mSwitchH},
		label:   label,
		tip:     tip,
		key:     key,
		enabled: true,
		checked: on,
	})
}

// caption lays out a read-only line. tip, when set, is shown as the native
// tooltip — which is how a truncated path still reveals itself in full.
func (b *uiBuilder) caption(icon, text string, role int, tip string) {
	top := b.rowStart(mTextH)
	b.items = append(b.items, uiItem{
		kind:  itCaption,
		rect:  walk.Rectangle{X: b.x + mCardPad, Y: top, Width: b.w - 2*mCardPad, Height: mTextH},
		label: text,
		icon:  icon,
		tip:   tip,
		role:  role,
	})
}

// divider lays out a hairline, used to separate groups of actions inside a card.
// It sits on the card's own background, so it reads as a seam rather than a box.
func (b *uiBuilder) divider() {
	top := b.rowStart(mSepH)
	b.items = append(b.items, uiItem{
		kind: itDivider,
		rect: walk.Rectangle{X: b.x + mCardPad, Y: top, Width: b.w - 2*mCardPad, Height: mSepH},
	})
}

// meta lays out a label on the left and a value pinned to the right edge on the
// same row, with the value in a fixed slot.
//
// This is what keeps the typographic hierarchy of the playback line intact: the
// title is strong and ellipsised, the time is small and muted, and a long file
// name can never push the time out of the card or stretch it.
func (b *uiBuilder) meta(icon, label, value string) {
	top := b.rowStart(mTextH + 3)
	inner := b.w - 2*mCardPad

	b.items = append(b.items, uiItem{
		kind:  itCaption,
		rect:  walk.Rectangle{X: b.x + mCardPad + inner - metaWidth, Y: top, Width: metaWidth, Height: mTextH + 3},
		label: value,
		role:  capTiny,
		right: true,
	})

	b.items = append(b.items, uiItem{
		kind:  itCaption,
		rect:  walk.Rectangle{X: b.x + mCardPad, Y: top, Width: inner - metaWidth - 14, Height: mTextH + 3},
		label: label,
		icon:  icon,
		role:  capStrong,
	})
}

// finish closes the card, fixes its height and publishes its items.
//
// It returns the card's index in u.cards rather than its height, so a caller can
// pair the card with its neighbour and give both the same height. Returning the
// height made the two interchangeable at the call site, which is exactly the
// mistake that indexed u.cards with a pixel count and took the process down
// before the window appeared.
func (b *uiBuilder) finish() int {
	b.h += mCardPad
	idx := len(b.u.cards) - 1
	b.u.cards[idx].rect.Height = b.h
	b.u.items = append(b.u.items, b.items...)
	return idx
}

// ---------------------------------------------------------------------------
// canvas
// ---------------------------------------------------------------------------

type uiCanvas struct {
	cw *walk.CustomWidget

	// pointer state, in canvas pixels
	mouseX, mouseY int
	mouseIn        bool
	hoverIdx       int
	pressIdx       int

	// Native tooltips are intentionally not updated during hover: Walk sends
	// synchronous messages to the tooltip window and that path caused flicker.
	focusIdx int
	// focusVisible is true only after the keyboard moved the focus. A ring that
	// stays on a button after it was clicked with the mouse reads as a selected
	// state, which it is not.
	focusVisible bool
	// leaveArmed is set while a pointer-left-the-window check is pending.
	leaveArmed bool

	scroll  int
	viewH   int
	content int

	// scrollbar geometry, recomputed in paintScrollbar so the mouse handlers can
	// drag and click it without re-deriving the layout.
	scrollbarX, scrollbarW           int
	scrollbarTrackY, scrollbarTrackH int
	scrollbarThumbY, scrollbarThumbH int
	scrollbarSize                    walk.Size
	scrollbarHas                     bool
	scrollDrag                       bool
	scrollDragStartY                 int
	scrollDragStartScroll            int

	// rebuilt on every paint
	items []uiItem
	cards []uiCard

	// header chrome
	headerRect     walk.Rectangle
	headerDividerY int

	// live readouts, refreshed at the top of every paint
	status   uiStateCopy
	nowTitle string
	nowMeta  string
	mpvPath  string

	// paintedSig identifies the frame currently on screen, so the player poll
	// can skip repaints that would look identical.
	paintedSig string
	// paintedStructSig is the same identity without the playback position. The
	// position moves every poll, so comparing only this tells whether anything
	// structural changed, and the position alone can be repainted in place
	// instead of redrawing the whole window.
	paintedStructSig string
}

// Win32 window styles for the canvas. Spelled out so the project needs no
// direct dependency on github.com/lxn/win for three constants.
const (
	wsChildWindow  = 0x40000000
	wsVisible      = 0x10000000
	wsClipChildren = 0x02000000
	wsCanvasStyle  = wsChildWindow | wsVisible | wsClipChildren
)

// ScrollWindowEx flags and the lazy proc. Scrolling a window by shifting its
// pixels in place (instead of repainting everything) is what keeps a drag on
// the scrollbar from flickering: only the newly exposed strip is repainted.
var procScrollWindowEx = windows.NewLazySystemDLL("user32.dll").NewProc("ScrollWindowEx")

const (
	swInvalidate = 0x0002
)

func newUICanvas(parent walk.Container) *uiCanvas {
	u := &uiCanvas{hoverIdx: -1, pressIdx: -1, focusIdx: -1}

	cw, err := walk.NewCustomWidgetPixels(parent, wsCanvasStyle,
		func(c *walk.Canvas, bounds walk.Rectangle) error {
			return u.paint(c, bounds)
		})
	if err != nil {
		return nil
	}
	u.cw = cw

	loadUIFonts(cw.DPI())

	// The window is one opaque surface that paint() always redraws in full, so
	// it must never be blanked beforehand.
	//
	// walk's default is PaintNormal, which lets WM_ERASEBKGND fill the whole
	// client before WM_PAINT runs. Those are two separate messages, and the
	// desktop compositor can sample the window in between — the user sees the
	// window go blank and come back, once per hover change plus once per the
	// 750 ms player poll. PaintNoErase suppresses the erase; paint() fills the
	// background itself as its first draw.
	//
	// PaintBuffered is the other way to avoid the erase, and walk offers it
	// precisely for this, but it is NOT usable here: bufferedPaint sizes its
	// offscreen bitmap to updateBounds while still painting and blitting at
	// updateBounds' absolute origin. That is self-consistent only when the
	// damage starts at (0,0), which is exactly what the partial invalidation
	// below stops guaranteeing. Single-buffered it is.
	cw.SetPaintMode(walk.PaintNoErase)

	// Insurance for any erase walk or Windows decides to send on its own: it
	// must be our colour, not the system window colour.
	cw.SetBackground(uiBrush(cBackground))

	// The layout is derived from the client size, so a resize needs a repaint.
	// Without this a grown window would keep the old layout in the part that was
	// not newly exposed.
	cw.SetInvalidatesOnResize(true)
	cw.SetCursor(walk.CursorArrow())

	cw.MouseMove().Attach(u.onMouseMove)
	cw.MouseDown().Attach(u.onMouseDown)
	cw.MouseUp().Attach(u.onMouseUp)

	return u
}

// InvalidateSwitch repaints only the switch bound to a setting.
//
// A toggle click changes one control and nothing else in the window: its own
// track and knob. Going through InvalidateIfChanged for it would dirty the whole
// surface for a 40x22 change.
func (u *uiCanvas) InvalidateSwitch(key string) {
	if u == nil || u.cw == nil || key == "" {
		return
	}
	for i := range u.items {
		it := &u.items[i]
		if it.kind == itSwitch && it.key == key {
			// Only the region is dirtied. paint() rebuilds every item from the
			// store, so the new state appears on its own — nothing is flipped here,
			// which would be wrong if the store write had been rejected.
			u.invalidateRects(u.damageFor(i))
			return
		}
	}
	// No switch carries that key in the current layout. Nothing above depends on
	// this being a partial repaint, so the full surface is the correct answer
	// rather than a row that never catches up.
	u.Invalidate()
}

// damageFor is the region that must be repainted when a control's appearance
// changes, padded for the focus ring and the 1 px borders drawn just outside the
// fill. An out-of-range index yields a zero rectangle, which invalidateRects
// skips.
func (u *uiCanvas) damageFor(idx int) walk.Rectangle {
	if idx < 0 || idx >= len(u.items) {
		return walk.Rectangle{}
	}
	r := u.items[idx].rect
	if r.Width <= 0 || r.Height <= 0 {
		return walk.Rectangle{}
	}
	// 4 px covers uiExpand(r, 2) plus a one-pixel bleed.
	return uiExpand(uiInset(r, -4), 0)
}

// Invalidate schedules a repaint. Walk-thread only — background goroutines go
// through refreshGuiSafe in window.go, which marshals first.
func (u *uiCanvas) Invalidate() {
	if u == nil || u.cw == nil {
		return
	}
	_ = u.cw.Invalidate()
}

// procInvalidateRect is called directly because walk exposes no way to dirty a
// sub-rectangle: WindowBase.Invalidate always uses InvalidateRect(hWnd, nil,
// true) — the entire client, plus an erase.
var procInvalidateRect = windows.NewLazySystemDLL("user32.dll").NewProc("InvalidateRect")

// invalidateRects marks only the given rectangles as needing a repaint.
//
// This is the difference between redrawing 1.4 million pixels and redrawing two
// 40x32 ones. It matters because the update region is also what the paint
// function uses to cull work, and because a small damage region keeps the
// window's own surface from being rewritten end to end on every control the
// cursor brushes past.
//
// bErase is 0 on purpose: paint() fills its own background as its first
// command, so asking Windows to erase first would only add a second full write
// of the same pixels.
func (u *uiCanvas) invalidateRects(rects ...walk.Rectangle) {
	if u == nil || u.cw == nil {
		return
	}
	if procInvalidateRect.Find() != nil {
		// Cannot happen on any supported Windows build, but a full repaint is a
		// correct fallback rather than a wrong picture.
		u.Invalidate()
		return
	}
	h := uintptr(u.cw.Handle())
	for _, r := range rects {
		if r.Width <= 0 || r.Height <= 0 {
			continue
		}
		rc := [4]int32{int32(r.X), int32(r.Y), int32(r.X + r.Width), int32(r.Y + r.Height)}
		procInvalidateRect.Call(h, uintptr(unsafe.Pointer(&rc[0])), 0)
		runtime.KeepAlive(&rc)
	}
}

// uiToggleKeys is every persisted boolean the window displays. It drives both
// the switches and the repaint signature, so the two can never diverge. Only
// settings with a visible control belong here: one without a switch would keep
// the signature churning on every poll for a change nobody can see.
var uiToggleKeys = [...]string{
	"aniListTrack",
	"fillerWarn",
	"malTrack",
	"simulJoinSync",
	"runOnStartup",
}

// signatureFrom is the identity of everything the window shows. Two states with
// the same signature paint identically, so one of them never needs painting.
func (u *uiCanvas) signatureFrom(status, title, meta, mpvPath string) string {
	var b strings.Builder
	b.Grow(192)
	b.WriteString(status)
	b.WriteByte(0)
	b.WriteString(title)
	b.WriteByte(0)
	b.WriteString(meta)
	b.WriteByte(0)
	b.WriteString(mpvPath)
	b.WriteByte(0)
	b.WriteString(accountLine("anilist"))
	b.WriteByte(0)
	b.WriteString(accountLine("mal"))
	b.WriteByte(0)
	for _, k := range uiToggleKeys {
		if storeToggle(k) {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	b.WriteByte(0)
	for _, a := range uiGatedActions {
		if uiActionEnabled(a) {
			b.WriteByte('1')
		} else {
			b.WriteByte('0')
		}
	}
	return b.String()
}

// InvalidateIfChanged repaints only when the window would actually look
// different.
//
// This is the path the 750 ms player poll takes. walk's Invalidate always
// dirties the entire surface, so repainting on a timer regardless would redraw
// a 1280x1100 window several times a second to produce an identical frame —
// work, and with any erase in the path, a visible blink. Walk thread only.
func (u *uiCanvas) InvalidateIfChanged() {
	if u == nil || u.cw == nil {
		return
	}
	st := mpvStatus()
	title := nowPlayingTitle()
	meta := nowPlayingMeta()
	mpvPath := uiMpvDisplayValue()

	// Structural identity: everything the window shows except the playback
	// position. Only this decides whether a full repaint is warranted.
	structSig := u.signatureFrom(st.text, title, "", mpvPath)
	if structSig != u.paintedStructSig {
		// Something structural moved: status, title, mpv path, accounts or a
		// toggle. The whole surface has to be rebuilt.
		u.paintedStructSig = structSig
		u.paintedSig = u.signatureFrom(st.text, title, meta, mpvPath)
		u.Invalidate()
		return
	}

	// Only the position ticked. Repainting the whole window for a number that
	// moves once a second is what made the Reprodução card flicker: every card,
	// every label and the background were redrawn inside one WM_PAINT. Painting
	// just the card that shows the position keeps the rest of the surface
	// untouched.
	fullSig := u.signatureFrom(st.text, title, meta, mpvPath)
	if fullSig == u.paintedSig {
		return
	}
	u.paintedSig = fullSig
	if len(u.cards) == 0 {
		u.Invalidate()
		return
	}
	// The Reprodução card is the first one appended in computeLayout. Its rect
	// is the one from the last paint, which is close enough for a damage region:
	// paint() recomputes everything anyway, and a slightly generous box only
	// costs a few extra pixels.
	u.invalidateRects(u.cards[0].rect)
}

// measure returns the height the layout needs at the given width, without a
// visible window. runMainWindow uses it to size the window to its content, so
// there is no dead band at the bottom and no control gets cut.
func (u *uiCanvas) measure(width int) int {
	savedView, savedScroll := u.viewH, u.scroll
	u.viewH, u.scroll = 0, 0
	u.computeLayout(width)
	h := u.content
	u.viewH, u.scroll = savedView, savedScroll
	return h
}

// ---------------------------------------------------------------------------
// input
// ---------------------------------------------------------------------------

func (u *uiCanvas) hitAt(x, y int) int {
	for i := range u.items {
		k := u.items[i].kind
		if (k != itButton && k != itSwitch) || !u.items[i].enabled {
			continue
		}
		r := u.items[i].rect
		if x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height {
			return i
		}
	}
	return -1
}

// onMouseMove is the hottest path in the window: Windows delivers one message
// per movement, coalesced but still dozens to hundreds per second.
//
// It therefore does only three cheap things — record the position, hit-test, and
// notice whether the answer changed. Everything that costs anything (tooltip
// message, cursor, repaint) lives in applyHover, which runs once per control
// boundary crossed instead of once per event.
func (u *uiCanvas) onMouseMove(x, y int, _ walk.MouseButton) {
	u.mouseX, u.mouseY, u.mouseIn = x, y, true

	if u.scrollDrag {
		maxScroll := u.content - u.viewH
		if maxScroll > 0 && u.scrollbarTrackH > u.scrollbarThumbH {
			delta := y - u.scrollDragStartY
			// Map pixels of thumb travel to units of content scroll.
			travel := u.scrollbarTrackH - u.scrollbarThumbH
			newScroll := u.scrollDragStartScroll + delta*maxScroll/travel
			newScroll = uiMinInt(uiMaxInt(0, newScroll), maxScroll)
			u.scrollBy(newScroll - u.scroll)
		}
		return
	}

	if idx := u.hitAt(x, y); idx != u.hoverIdx {
		u.applyHover(idx)
	}
}

// applyHover is the only place the hovered control changes.
//
// The previous version called ToolTipText() on every mouse event, and that is
// not a getter: WidgetBase.ToolTipText goes through Tooltip.toolInfo, which
// allocates a 2 KB uint16 buffer and sends TTM_GETTOOLINFO — a *synchronous*
// SendMessage to the tooltip window — for every single call. That is hundreds
// of blocking cross-window messages per second on the UI thread, and a
// synchronous SendMessage lets the tooltip's window procedure run in the middle
// of the main window's own message handling, which is precisely the flicker the
// user reported. Comparing against hoverTip avoids the call entirely while the
// cursor stays inside one control.
func (u *uiCanvas) applyHover(idx int) {
	prev := u.hoverIdx
	u.hoverIdx = idx

	if idx >= 0 {
		u.cw.SetCursor(walk.CursorHand())
		u.armLeaveCheck()
	} else {
		u.cw.SetCursor(walk.CursorArrow())
	}

	// Repaint only the two rectangles that can look different: the control just
	// left and the one just entered. The focus ring is drawn outside the control
	// rect, so it is included by the padding below.
	u.invalidateRects(u.damageFor(prev), u.damageFor(idx))
}

var (
	procGetCursorPos   = windows.NewLazySystemDLL("user32.dll").NewProc("GetCursorPos")
	procScreenToClient = windows.NewLazySystemDLL("user32.dll").NewProc("ScreenToClient")
)

// cursorInCanvas reports whether the pointer is over the canvas right now. When
// it cannot tell it says yes, which only means a hover highlight lingers.
func (u *uiCanvas) cursorInCanvas() bool {
	if u.cw == nil || procGetCursorPos.Find() != nil || procScreenToClient.Find() != nil {
		return true
	}
	var pt [2]int32
	if r, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt[0]))); r == 0 {
		return true
	}
	_, _, _ = procScreenToClient.Call(uintptr(u.cw.Handle()), uintptr(unsafe.Pointer(&pt[0])))
	runtime.KeepAlive(&pt)
	sz := u.cw.ClientBoundsPixels().Size()
	return int(pt[0]) >= 0 && int(pt[1]) >= 0 && int(pt[0]) < sz.Width && int(pt[1]) < sz.Height
}

// armLeaveCheck notices the pointer leaving the window. walk's CustomWidget
// reports no mouse-leave, so the control last hovered stayed highlighted (and
// the hand cursor logic stale) until the pointer came back. While a control is
// hovered this polls a few times a second; once nothing is hovered it stops.
func (u *uiCanvas) armLeaveCheck() {
	if u.leaveArmed || u.cw == nil {
		return
	}
	u.leaveArmed = true
	time.AfterFunc(150*time.Millisecond, func() {
		u.cw.Synchronize(func() {
			u.leaveArmed = false
			if !u.cursorInCanvas() {
				u.mouseIn = false
				if u.hoverIdx >= 0 {
					u.applyHover(-1)
				}
				return
			}
			if u.hoverIdx >= 0 {
				u.armLeaveCheck()
			}
		})
	})
}

// onMouseDown arms a click; the action runs on release and only when the pointer
// is still inside the control, which is the standard button contract.
func (u *uiCanvas) onMouseDown(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton {
		return
	}

	// Scrollbar: drag the thumb or jump a page on the track.
	if u.scrollbarHas && x >= u.scrollbarX && x < u.scrollbarX+u.scrollbarW {
		if y >= u.scrollbarThumbY && y < u.scrollbarThumbY+u.scrollbarThumbH {
			u.scrollDrag = true
			u.scrollDragStartY = y
			u.scrollDragStartScroll = u.scroll
			return
		}
		if y >= u.scrollbarTrackY && y < u.scrollbarTrackY+u.scrollbarTrackH {
			maxScroll := u.content - u.viewH
			if maxScroll > 0 {
				if y < u.scrollbarThumbY {
					u.scrollBy(-(u.viewH - 40))
				} else {
					u.scrollBy(u.viewH - 40)
				}
			}
			return
		}
	}

	idx := u.hitAt(x, y)
	u.focusVisible = false
	// The focus ring moves: the control that had it gives it up and the one
	// clicked takes it. Dirtying only the clicked control left the old ring on
	// screen until something unrelated happened to repaint over it.
	prevFocus := u.focusIdx
	u.pressIdx = idx
	if idx >= 0 {
		u.focusIdx = idx
	}
	u.invalidateRects(u.damageFor(prevFocus), u.damageFor(idx))
}

func (u *uiCanvas) onMouseUp(x, y int, button walk.MouseButton) {
	if button != walk.LeftButton {
		return
	}
	if u.scrollDrag {
		u.scrollDrag = false
		return
	}
	down := u.pressIdx
	u.pressIdx = -1
	idx := u.hitAt(x, y)
	// The control that was held and the one released inside share one repaint.
	u.invalidateRects(u.damageFor(down), u.damageFor(idx))

	if down >= 0 && idx == down {
		u.activate(idx)
	}
}

// activate performs the action bound to a laid-out item.
func (u *uiCanvas) activate(idx int) {
	if idx < 0 || idx >= len(u.items) {
		return
	}
	it := u.items[idx]
	if !it.enabled {
		return
	}
	switch it.kind {
	case itButton:
		if it.action != "" {
			guiAction(it.action)
		}
		refreshGui()
	case itSwitch:
		// guiSetToggle dirties this switch and nothing else, and no blanket
		// refresh follows. The switch states are part of the paint signature, so
		// a refreshGui() here would always find a difference and repaint the
		// whole window: the surface, all six cards, every label, inside a single
		// WM_PAINT. On a click that moves one 40x22 thumb, that is the flicker.
		//
		// The partial paint still updates the signature, because paint() reads
		// live state at its top, so the 750 ms poll then sees nothing left to do.
		guiSetToggle(it.key, !it.checked)
	}
}

// onMouseWheel scrolls the content when it does not fit.
//
// It is attached to the MainWindow, not to the canvas: Windows delivers
// WM_MOUSEWHEEL to the window that holds keyboard focus, and with nothing
// focusable in this window that is the form. Attaching it to the canvas would
// silently never fire. x and y are screen coordinates for this message and are
// deliberately unused.
func (u *uiCanvas) onMouseWheel(x, y int, button walk.MouseButton) {
	maxScroll := u.content - u.viewH
	if maxScroll <= 0 {
		return
	}

	// walk hands us the whole wParam: the wheel delta lives in the high word and
	// the key state in the low one (see MouseWheelEventDelta). One notch is
	// +/-WHEEL_DELTA (120); a larger movement is several notches at once, so the
	// scroll distance scales with the delta instead of being a fixed 54 px.
	delta := walk.MouseWheelEventDelta(button)
	if delta == 0 {
		return
	}
	u.scrollBy(-delta / 2)
}

// scrollBy moves the content by delta pixels. ScrollWindowEx shifts the pixels
// already on screen in place, so only the newly exposed strip has to be
// repainted: a drag on the scrollbar then scrolls smoothly instead of
// redrawing every card on every mouse move, which is what flickered.
func (u *uiCanvas) scrollBy(delta int) {
	if delta == 0 || u.cw == nil {
		return
	}
	maxScroll := u.content - u.viewH
	if maxScroll <= 0 {
		return
	}
	newScroll := uiMinInt(uiMaxInt(0, u.scroll+delta), maxScroll)
	if newScroll == u.scroll {
		return
	}
	actual := newScroll - u.scroll
	u.scroll = newScroll

	hwnd := uintptr(u.cw.Handle())
	if procScrollWindowEx.Find() == nil {
		var rc [4]int32
		procScrollWindowEx.Call(hwnd, 0, uintptr(-actual), 0, 0, 0, uintptr(unsafe.Pointer(&rc[0])), swInvalidate)
		runtime.KeepAlive(&rc)
	} else {
		u.Invalidate()
		return
	}

	// The scrollbar thumb moved; make sure that strip is repainted too.
	if u.scrollbarHas {
		u.invalidateRects(walk.Rectangle{
			X: u.scrollbarX, Y: u.scrollbarTrackY,
			Width: u.scrollbarW, Height: u.scrollbarTrackH,
		})
	}
}

// ensureVisible scrolls just enough to bring a control fully into view.
func (u *uiCanvas) ensureVisible(idx int) {
	if idx < 0 || idx >= len(u.items) {
		return
	}
	r := u.items[idx].rect
	margin := uiMetric(16)
	switch {
	case r.Y < margin:
		u.scrollBy(r.Y - margin)
	case r.Y+r.Height > u.viewH-margin:
		u.scrollBy(r.Y + r.Height - (u.viewH - margin))
	}
}

// OnKey handles Tab navigation and activation. It is attached to the MainWindow
// rather than the canvas: walk publishes key events to whichever window holds
// focus, and the canvas is not focusable.
func (u *uiCanvas) OnKey(key walk.Key) {
	step := 1
	if walk.ModifiersDown()&walk.ModShift != 0 {
		step = -1
	}

	switch key {
	case walk.KeyEscape:
		if u.scroll != 0 {
			u.scrollBy(-u.scroll)
		} else {
			hideMainWindow()
		}

	case walk.KeyTab:
		// Only the controls take part in the tab order; captions and status
		// lines are drawn, not operated.
		var order []int
		for i := range u.items {
			if k := u.items[i].kind; (k == itButton || k == itSwitch) && u.items[i].enabled {
				order = append(order, i)
			}
		}
		if len(order) == 0 {
			return
		}

		pos := -1
		for i, idx := range order {
			if idx == u.focusIdx {
				pos = i
				break
			}
		}
		if pos < 0 && step < 0 {
			// Nothing focused yet: Tab starts at the first control and
			// Shift+Tab at the last one.
			pos = len(order)
		}
		u.focusIdx = order[(pos+step+len(order))%len(order)]
		u.focusVisible = true
		// A control scrolled out of view cannot be seen to have the focus.
		u.ensureVisible(u.focusIdx)
		u.Invalidate()

	case walk.KeyReturn, walk.KeySpace:
		if u.focusIdx >= 0 && u.focusIdx < len(u.items) {
			if !u.focusVisible {
				u.focusVisible = true
				u.invalidateRects(u.damageFor(u.focusIdx))
			}
			u.activate(u.focusIdx)
		}

	case walk.Key(0x21): // VK_PRIOR
		u.scrollBy(-(u.viewH - uiMetric(40)))

	case walk.KeyUp:
		u.scrollBy(-uiMetric(48))

	case walk.Key(0x22): // VK_NEXT
		u.scrollBy(u.viewH - uiMetric(40))

	case walk.KeyDown:
		u.scrollBy(uiMetric(48))

	case walk.KeyHome:
		u.scrollBy(-u.scroll)

	case walk.KeyEnd:
		maxScroll := u.content - u.viewH
		if maxScroll > 0 {
			u.scrollBy(maxScroll - u.scroll)
		}
	}
}

// ---------------------------------------------------------------------------
// live state
// ---------------------------------------------------------------------------

// storeToggle reads one boolean setting under the store lock.
func storeToggle(key string) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	switch key {
	case "aniListTrack":
		return store.AniListTracking
	case "fillerWarn":
		return store.WarnFiller
	case "malTrack":
		return store.MALTracking
	case "simulJoinSync":
		return store.SimulJoinSync
	case "runOnStartup":
		return store.RunOnStartup
	}
	return false
}

// storeAccount returns the connected user name for a service, or "".
func storeAccount(service string) string {
	store.mu.RLock()
	defer store.mu.RUnlock()
	if service == "mal" {
		return strings.TrimSpace(store.MALUsername)
	}
	return strings.TrimSpace(store.Username)
}

// accountLine renders a service's connection state for the repaint signature.
// The account row itself is omitted when there is nothing to show, but the
// signature still has to change when a login succeeds.
func accountLine(service string) string {
	if name := storeAccount(service); name != "" {
		return name
	}
	return ""
}

// uiStateCopy describes the window's single global state indicator.
//
// One fixed width for every state, sized for the longest caption. Widths per
// state made the pill resize as the state changed, and the estimate for the
// longer ones was short enough to truncate "Desconectado" to "Desconec...". A
// constant width cannot truncate and cannot jiggle.
type uiStateCopy struct {
	text  string
	color walk.Color
	icon  string
}

// statusPillW fits the longest caption, "Desconectado", with room to spare.
// It is recomputed per DPI by loadUIFonts; the value here is the 96 dpi default.

// mpvStatus is the one place the player state is put into words, so the header
// pill and the Reprodução card can never disagree.
//
// Named mpvStatus rather than mpvState because main.go already declares mpvState
// as the IPC snapshot type filled in by mpv.go.
func mpvStatus() uiStateCopy {
	if st, ok := sharedMpvConn.Snapshot(); ok {
		switch {
		case st.Pause && st.Duration > 0:
			return uiStateCopy{"Pausado", cWarn, iconPause}
		case st.Duration > 0:
			return uiStateCopy{"Reproduzindo", cOK, iconPlay}
		default:
			return uiStateCopy{"Conectado", cAccent, iconCheck}
		}
	}
	// Connected but nothing loaded: Snapshot reports no state for an idle player,
	// so this branch is the only way "Conectado" is ever reached. Without it an
	// idle mpv that Kouki had launched read "Iniciando…" forever, and one started
	// by hand read "Desconectado" while the channel was up.
	if sharedMpvConn.Connected() {
		return uiStateCopy{"Conectado", cAccent, iconCheck}
	}
	if guiPlayer.IsRunning() {
		return uiStateCopy{"Iniciando…", cWarn, iconSync}
	}
	return uiStateCopy{"Desconectado", cTextDim, iconStop}
}

// nowPlayingTitle is the left half of the Reprodução row: what is playing, or
// why nothing is.
func nowPlayingTitle() string {
	if st, ok := sharedMpvConn.Snapshot(); ok {
		if title := firstNonEmpty(st.MediaTitle, st.FileName, st.Path); title != "" {
			return title
		}
	}
	if sharedMpvConn.Connected() {
		return "Player conectado — nenhum arquivo aberto"
	}
	if guiPlayer.IsRunning() {
		return "Player iniciado — aguardando o canal"
	}
	return "Nenhum player conectado"
}

// nowPlayingMeta is the right half of the same row: position and total when a
// file is loaded.
//
// It returns an empty string when there is nothing to say, and the slot then
// draws nothing at all. A dash or a placeholder there would be exactly the empty
// indicator the layout is trying to avoid.
func nowPlayingMeta() string {
	st, ok := sharedMpvConn.Snapshot()
	if !ok || st.Duration <= 0 {
		return ""
	}
	return formatDuration(st.TimePos) + " / " + formatDuration(st.Duration)
}

// uiMpvDisplay is the "which mpv.exe is in use" line of the Reprodução card.
//
// It is memoised on purpose. resolveMpvPath() falls through to
// FindMpvCandidates(), and that runs `mpv.exe --version` against every candidate
// to read its version — so calling it from the repaint loop, which runs twice a
// second, would launch a process on every frame. The value is resolved on first
// read and refreshed only by invalidateUIMpvDisplay(), at the handful of points
// where the answer can genuinely have changed.
var uiMpvDisplay struct {
	mu    sync.Mutex
	value string
}

func uiMpvDisplayValue() string {
	uiMpvDisplay.mu.Lock()
	defer uiMpvDisplay.mu.Unlock()
	if uiMpvDisplay.value == "" {
		uiMpvDisplay.value = describeMpvPath(resolveMpvPath())
	}
	return uiMpvDisplay.value
}

// invalidateUIMpvDisplay drops the memo so the next read resolves again.
func invalidateUIMpvDisplay() {
	uiMpvDisplay.mu.Lock()
	uiMpvDisplay.value = ""
	uiMpvDisplay.mu.Unlock()
}

// ---------------------------------------------------------------------------
// layout
// ---------------------------------------------------------------------------

func (u *uiCanvas) computeLayout(w int) {
	u.cards = u.cards[:0]
	u.items = u.items[:0]

	if w < 380 {
		w = 380
	}
	contentW := w - 2*mPad

	// One column below this width. The threshold is the narrowest a column can be
	// while its buttons and switch labels still fit without being clipped, so a
	// resize degrades to a single stack instead of squeezing two.
	colW := (contentW - mColGap) / 2
	twoColumns := colW >= mMinColW
	if !twoColumns {
		colW = contentW
	}

	u.headerRect = walk.Rectangle{X: mPad, Y: mPad, Width: contentW, Height: mHeaderH}
	u.headerDividerY = mPad + mHeaderH + mGap/2

	y := mPad + mHeaderH + mGap

	y += u.cards[u.cardPlayback(mPad, y, contentW)].rect.Height + mGap

	if twoColumns {
		// AniList and MyAnimeList pair up because they are the same shape: both
		// come from accountCard, both offer login, logout and track. Simulwatching
		// is a session card rather than an account card, so it takes the full
		// width below them, the way Reprodução and Geral do. A third column would
		// have needed a filler, and a half-width hole in the grid reads as a
		// layout mistake.
		a := u.cardAniList(mPad, y, colW)
		b := u.cardMAL(mPad+colW+mColGap, y, colW)
		y += u.stretchPair(a, b) + mGap

		y += u.cards[u.cardSimul(mPad, y, contentW)].rect.Height + mGap
	} else {
		for _, build := range []func(x, y, w int) int{u.cardAniList, u.cardMAL, u.cardSimul} {
			y += u.cards[build(mPad, y, colW)].rect.Height + mGap
		}
	}

	u.cardGeneral(mPad, y, contentW)
	u.content = y + mPad
	u.scroll = uiMinInt(uiMaxInt(0, u.scroll), uiMaxInt(0, u.content-u.viewH))

	for i := range u.cards {
		u.cards[i].rect.Y -= u.scroll
	}
	for i := range u.items {
		u.items[i].rect.Y -= u.scroll
	}
	// The header scrolls with everything else. It was left at a fixed position
	// while ScrollWindowEx shifted its pixels, and the cards (painted after it)
	// slid over it: on a short screen the wordmark and the status pill were
	// overdrawn, and the next full repaint snapped the header back.
	u.headerRect.Y -= u.scroll
	u.headerDividerY -= u.scroll
}

// stretchPair gives two sibling cards the height of the taller one and returns
// it. Each card was appended to u.cards immediately before, so the shorter one
// only grows its background: the rows inside keep their positions and the card
// simply ends flush with its neighbour.
//
// The indices come from uiBuilder.finish; passing anything else here indexes
// u.cards out of range and panics inside computeLayout.
func (u *uiCanvas) stretchPair(a, b int) int {
	if a < 0 || b < 0 || a >= len(u.cards) || b >= len(u.cards) {
		return 0
	}
	h := uiMaxInt(u.cards[a].rect.Height, u.cards[b].rect.Height)
	u.cards[a].rect.Height = h
	u.cards[b].rect.Height = h
	return h
}

func (u *uiCanvas) cardPlayback(x, y, w int) int {
	b := u.newCard(x, y, w, iconPlay, "Reprodução", "Player e canal de comunicação")

	// One row, one hierarchy: what is playing on the left in the strong face,
	// position and total pinned right in the small muted face. The connection
	// state is NOT repeated here — the header pill is the single indicator.
	b.meta(iconAccount, u.nowTitle, u.nowMeta)

	// "Iniciar MPV" is the only accent on this card. Pausing is a secondary
	// action; stopping is given its own hue so it reads as different without
	// becoming a red slab, which is what a filled danger button would look like.
	b.buttons(mBtnH,
		uiButtonSpec{
			label: "Iniciar MPV", action: "mpv-launch", variant: bvPrimary, weight: 1,
			tip: "Abre o mpv com o canal IPC configurado pelo Kouki",
		},
		uiButtonSpec{
			label: "Pausar / Retomar", action: "mpv-toggle-pause", weight: 1,
			tip: "Alterna a reprodução no player conectado",
		},
		uiButtonSpec{
			label: "Encerrar MPV", action: "mpv-stop", variant: bvDanger, weight: 1,
			tip: "Fecha o player que o Kouki iniciou",
		},
	)

	// The path is truncated to fit, and the native tooltip carries the full text.
	b.caption(iconFolder, u.mpvPath, capTiny, u.mpvPath)

	// Only the manual scan stays. Detecting and installing the player is already
	// automatic at startup when no MPV is found, so offering both as buttons put
	// two ways of doing the same thing next to each other, and "Usar encontrado"
	// duplicated the automatic pick on top of that.
	b.buttons(mToolH,
		uiButtonSpec{
			label: "Procurar MPV", action: "mpv-detect", weight: 1,
			tip: "Varre os locais padrão e o PATH",
		},
	)
	return b.finish()
}

// accountCard is the shared shape of the AniList and MyAnimeList cards. Both are
// built by the same function so their internal spacing, row heights and button
// proportions cannot drift apart, which is exactly what the eye notices when
// they sit side by side. toggles is {setting key, label, tooltip}.
func (u *uiCanvas) accountCard(x, y, w int, icon, title, service, login, logout, correct string, toggles [][3]string) int {
	b := u.newCard(x, y, w, icon, title, "Conta e progresso")

	// Signed in, "Entrar" is no longer the thing to do: it becomes a quiet
	// "Reconectar" instead of a second accent button next to a connected account.
	loginLabel, loginVariant := "Entrar", bvPrimary
	if storeAccount(service) != "" {
		loginLabel, loginVariant = "Reconectar", bvSecondary
	}

	// The account row only exists when there is an account. Reserving a line for
	// "no account connected" pushed every preference down for nothing.
	if name := storeAccount(service); name != "" {
		b.caption(iconAccount, name, capMuted, "Conectado como "+name)
	}

	b.buttons(mBtnH,
		uiButtonSpec{
			label: loginLabel, action: login, variant: loginVariant, weight: 1,
			tip: "Autoriza o Kouki no navegador",
		},
		uiButtonSpec{
			label: "Sair", action: logout, weight: 1,
			tip: "Encerra a sessão",
		},
		uiButtonSpec{
			label: "Selecionar anime", action: correct, weight: 2,
			tip: "Corrige a série do que está tocando",
		},
	)
	for _, t := range toggles {
		b.toggle(t[0], t[1], t[2], storeToggle(t[0]))
	}
	return b.finish()
}

func (u *uiCanvas) cardAniList(x, y, w int) int {
	return u.accountCard(x, y, w, iconAccount, "AniList", "anilist",
		"ani-login", "ani-logout", "ani-correct",
		[][3]string{
			{"aniListTrack", "Atualizar progresso", "Sincroniza episódios e progresso"},
			{"fillerWarn", "Avisar sobre episódios filler", "Sinaliza episódios de preenchimento na presença"},
		})
}

func (u *uiCanvas) cardMAL(x, y, w int) int {
	return u.accountCard(x, y, w, iconAccount, "MyAnimeList", "mal",
		"mal-login", "mal-logout", "mal-correct",
		[][3]string{
			{"malTrack", "Atualizar progresso", "Sincroniza episódios e progresso"},
		})
}

func (u *uiCanvas) cardSimul(x, y, w int) int {
	b := u.newCard(x, y, w, iconSync, "Simulwatching", "Sessão compartilhada")
	b.buttons(mBtnH,
		uiButtonSpec{
			label: "Hospedar", action: "simul-host", variant: bvPrimary, weight: 1,
			tip: "Cria uma sala e mostra o código",
		},
		uiButtonSpec{
			label: "Entrar", action: "simul-join", weight: 1,
			tip: "Entra em uma sala por endereço e código",
		},
		uiButtonSpec{
			label: "Parar", action: "simul-stop", weight: 1,
			tip: "Encerra a sessão atual",
		},
	)
	b.toggle("simulJoinSync", "Sincronizar meu AniList",
		"Atualiza o progresso ao entrar na sala", storeToggle("simulJoinSync"))
	return b.finish()
}

func (u *uiCanvas) cardGeneral(x, y, w int) int {
	b := u.newCard(x, y, w, iconGear, "Geral", "Inicialização e manutenção")
	b.toggle("runOnStartup", "Executar ao iniciar o Windows",
		"Abre o Kouki na bandeja quando você entra no Windows", storeToggle("runOnStartup"))

	// A hairline separates the navigation group from the maintenance group, so
	// the two read as two blocks instead of six unrelated buttons. Both rows are
	// thirds: every button shares the same size and baseline.
	b.buttons(mToolH,
		uiButtonSpec{
			label: "Pasta do programa", action: "open-install-folder", weight: 1,
			tip: "Abre a pasta onde o Kouki está instalado",
		},
		uiButtonSpec{
			label: "Configurações", action: "open-config-folder", weight: 1,
			tip: "Abre a pasta de dados do Kouki",
		},
		uiButtonSpec{
			label: "Pasta do MPV", action: "open-mpv-folder", weight: 1,
			tip: "Abre a pasta do mpv.exe em uso",
		},
	)
	b.divider()
	b.buttons(mToolH,
		uiButtonSpec{
			label: "Reinstalar / Desinstalar", action: "reinstall", weight: 1,
			tip: "Abre o instalador do Kouki",
		},
		uiButtonSpec{
			label: "Sobre o Kouki", action: "about", weight: 1,
			tip: "Versão e informações do aplicativo",
		},
		uiButtonSpec{
			label: "Sair", action: "quit", variant: bvDanger, weight: 1,
			tip: "Encerra o Kouki e fecha o player",
		},
	)
	b.divider()

	// The version lived in the header until the mark and the wordmark became the
	// whole left side of it. It belongs with the maintenance actions anyway: this
	// is the card people open when they are about to report a problem.
	b.caption(iconInfo, "Versão "+appVersion, capTiny, "Versão "+appVersion)
	return b.finish()
}

// ---------------------------------------------------------------------------
// painting
// ---------------------------------------------------------------------------

func (u *uiCanvas) paint(c *walk.Canvas, bounds walk.Rectangle) error {
	// ClientBoundsPixels is the exact client area; BoundsPixels is the fallback
	// for a moment before the first size is known.
	size := u.cw.ClientBoundsPixels().Size()
	if size.Width <= 0 || size.Height <= 0 {
		size = u.cw.BoundsPixels().Size()
	}
	if size.Width <= 0 || size.Height <= 0 {
		size = bounds.Size()
	}
	if size.Width <= 0 || size.Height <= 0 {
		return nil
	}

	// Live readouts, refreshed here so nothing else has to remember to update.
	st := mpvStatus()
	u.status = st
	u.nowTitle = nowPlayingTitle()
	u.nowMeta = nowPlayingMeta()
	u.mpvPath = uiMpvDisplayValue()
	u.paintedSig = u.signatureFrom(st.text, u.nowTitle, u.nowMeta, u.mpvPath)
	u.paintedStructSig = u.signatureFrom(st.text, u.nowTitle, "", u.mpvPath)

	u.viewH = size.Height
	// Reset before computeLayout so a frame with no overflow clears the stale
	// geometry from the previous one.
	u.scrollbarHas = false
	u.computeLayout(size.Width)

	// Recomputed rather than remembered, so hover survives a relayout. Native
	// tooltip updates are intentionally disabled to keep painting re-entrancy-free.
	if u.mouseIn {
		u.hoverIdx = u.hitAt(u.mouseX, u.mouseY)
	} else {
		u.hoverIdx = -1
	}

	// bounds is the damaged region. Everything outside it is already correct and
	// GDI clips the drawing anyway, so culling here only saves the work of
	// issuing the calls. On a hover change this is a handful of controls instead
	// of the entire window.
	uiFill(c, walk.Rectangle{X: 0, Y: 0, Width: size.Width, Height: size.Height}, 0, cBackground)

	u.paintHeader(c, bounds)
	for i := range u.cards {
		if uiOverlaps(u.cards[i].rect, bounds) {
			u.paintCard(c, u.cards[i])
		}
	}
	for i := range u.items {
		if uiOverlaps(u.damageFor(i), bounds) {
			u.paintItem(c, i)
		}
	}
	if u.content > u.viewH {
		u.paintScrollbar(c, size)
	}

	return nil
}

// uiOverlaps reports whether two rectangles share any pixel.
func uiOverlaps(a, b walk.Rectangle) bool {
	if a.Width <= 0 || a.Height <= 0 || b.Width <= 0 || b.Height <= 0 {
		return false
	}
	return a.X < b.X+b.Width && b.X < a.X+a.Width &&
		a.Y < b.Y+b.Height && b.Y < a.Y+a.Height
}

func (u *uiCanvas) paintHeader(c *walk.Canvas, damage walk.Rectangle) {
	r := u.headerRect

	// One vertical rhythm for the whole strip, the equivalent of a flex row with
	// align-items:center: the mark and the two text lines centre on the strip's
	// own midline instead of floating independently. Every height is measured,
	// not guessed, so a scaled face cannot push the lines apart or clip them.
	nameH := uiLineHeight(c, uiFont(fAppName))
	tagH := uiLineHeight(c, uiFont(fTagline))
	gapText := uiMetric(2)

	blockH := nameH + gapText + tagH
	blockTop := r.Y + (r.Height-blockH)/2
	nameY := blockTop
	tagY := blockTop + nameH + gapText

	logo := walk.Rectangle{X: r.X, Y: r.Y + (r.Height-mLogo)/2, Width: mLogo, Height: mLogo}
	if bmp := uiLogo(mLogo); bmp != nil {
		_ = c.DrawImagePixels(bmp, walk.Point{X: logo.X, Y: logo.Y})
	} else {
		// Only if the artwork could not be decoded. A rounded tile is worse than
		// the mark, but it is better than an empty hole in the header.
		uiFill(c, logo, uiMetric(11), cAccent)
		uiTextCenter(c, "K", fLogo, uiMix(cAccent, cBackground, 0.85), logo)
	}

	tx := logo.X + logo.Width + uiMetric(14)
	uiTextLeft(c, appName, fAppName, cText,
		walk.Rectangle{X: tx, Y: nameY, Width: uiMetric(160), Height: nameH})
	uiTextLeft(c, appTagline, fTagline, cTextMuted,
		walk.Rectangle{X: tx, Y: tagY, Width: uiMetric(360), Height: tagH})

	u.paintStatusPill(c, r)

	divider := walk.Rectangle{X: r.X, Y: u.headerDividerY, Width: r.Width, Height: mSepH}
	if uiOverlaps(divider, damage) {
		uiFill(c, divider, 0, cBorder)
	}
}

// paintStatusPill is the window's single global state indicator.
//
// Each caption carries the pill width that fits it, because walk has no string
// measurement and a fixed-width box truncated the longer captions. States are
// distinguished by word *and* glyph as well as colour, so the indicator still
// reads without relying on hue.
// uiPillW is the width of the status pill, measured once from the widest caption.
// The old constant (196 px at 96 dpi) was about twice what "Desconectado" needs,
// which left the pill mostly empty; measuring keeps it snug at every DPI and
// still gives every state the same width, so it never jiggles.
var uiPillW int

func uiStatusPillWidth(c *walk.Canvas) int {
	if uiPillW > 0 {
		return uiPillW
	}
	widest := 0
	box := walk.Rectangle{Width: uiMetric(400), Height: uiMetric(100)}
	for _, s := range [...]string{"Reproduzindo", "Desconectado", "Iniciando…", "Conectado", "Pausado"} {
		b, _, err := c.MeasureTextPixels(s, uiFont(fMuted), box, walk.TextSingleLine|walk.TextNoPrefix)
		if err == nil && b.Width > widest {
			widest = b.Width
		}
	}
	if widest <= 0 {
		return statusPillW // measurement unavailable: use the fixed fallback, uncached
	}
	// left pad + dot + gap + text + right pad, with a few pixels of slack.
	uiPillW = uiMetric(12) + uiMetric(8) + uiMetric(8) + widest + uiMetric(14) + uiMetric(4)
	return uiPillW
}

func (u *uiCanvas) paintStatusPill(c *walk.Canvas, header walk.Rectangle) {
	st := u.status
	ph := uiMetric(32)
	dot := uiMetric(8)
	dotGap := uiMetric(8)
	padRight := uiMetric(14)
	pw := uiStatusPillWidth(c)
	box := walk.Rectangle{
		X:      header.X + header.Width - pw,
		Y:      header.Y + (header.Height-ph)/2,
		Width:  pw,
		Height: ph,
	}
	uiPanel(c, box, ph/2, uiTint(st.color, 0.13), uiTint(st.color, 0.30))

	uiRoundDot(c, box.X+uiMetric(12), box.Y+(ph-dot)/2, dot, st.color)
	// The coloured dot already communicates the state. A second glyph here
	// duplicated the signal and made the pill look cramped (especially for the
	// outlined stop glyph used by "Desconectado"). Keep the status label clean.
	label := walk.Rectangle{
		X:      box.X + uiMetric(12) + dot + dotGap,
		Y:      box.Y,
		Width:  pw - uiMetric(12) - dot - dotGap - padRight,
		Height: ph,
	}
	uiTextLeft(c, st.text, fMuted, uiMix(cText, st.color, 0.30), label)
}

func (u *uiCanvas) paintCard(c *walk.Canvas, card uiCard) {
	r := card.rect
	uiPanel(c, r, uiMetric(12), cSurface, cBorder)

	// One box for every card header, so the marks share size, weight and colour
	// whether the title is one word or two. The height is the larger of the title's
	// line box and the icon slot, so neither can clip the other.
	titleH := uiLineHeight(c, uiFont(fTitle))
	subH := uiLineHeight(c, uiFont(fSub))
	headH := uiMaxInt(uiMaxInt(titleH, mIconTitle), mTitleH)

	// Icon and title share one box and one baseline, and the box is at least as
	// tall as either face's line box.
	row := walk.Rectangle{
		X:      r.X + mCardPad,
		Y:      r.Y + mCardPad + (headH-titleH)/2,
		Width:  r.Width - 2*mCardPad,
		Height: titleH,
	}
	row = uiIcon(c, card.icon, cAccent, row, mIconTitle)
	uiTextLeft(c, card.title, fTitle, cText, row)

	if card.subtitle != "" {
		uiTextLeft(c, card.subtitle, fSub, cTextMuted,
			walk.Rectangle{
				X:      r.X + mCardPad,
				Y:      r.Y + mCardPad + headH + uiMetric(2),
				Width:  r.Width - 2*mCardPad,
				Height: subH,
			})
	}
}

func (u *uiCanvas) paintItem(c *walk.Canvas, idx int) {
	it := u.items[idx]
	switch it.kind {
	case itButton:
		u.paintButton(c, it, idx)
	case itSwitch:
		u.paintSwitch(c, it, idx)
	case itCaption:
		u.paintCaption(c, it)
	case itDivider:
		uiFill(c, it.rect, 0, cBorderSoft)
	}
}

// buttonSkin returns the fill, the label colour and the hairline for a button in
// its current state.
func buttonSkin(variant int, enabled, hover, pressed bool) (fill, text, border walk.Color) {
	switch variant {
	case bvPrimary:
		switch {
		case !enabled:
			return cSurfaceAlt, cTextDim, cBorder
		case pressed:
			return cAccentDown, cWhite, cBorder
		case hover:
			return cAccentHov, cWhite, cBorder
		default:
			return cAccent, cWhite, cBorder
		}
	case bvDanger:
		// Neutral at rest, like every other button in the group: a permanent red
		// outline made "Encerrar MPV" read as an alert. The hue only arrives on
		// hover, so it is an answer to the pointer rather than a warning at rest.
		switch {
		case !enabled:
			return cSurface, cTextDim, cBorder
		case hover:
			return uiMix(cSurfaceAlt, cErr, 0.16), cErr, uiTint(cErr, 0.55)
		case pressed:
			return uiMix(cSurface, cErr, 0.28), cErr, uiTint(cErr, 0.55)
		default:
			return cSurfaceAlt, cTextMuted, cBorder
		}
	case bvGhost:
		switch {
		case !enabled:
			return cSurface, cTextDim, cBorder
		case hover:
			return cSurfaceAlt, cText, cBorder
		default:
			return cSurface, cTextMuted, cBorder
		}
	default: // bvSecondary
		switch {
		case !enabled:
			return cSurface, cTextDim, cBorder
		case hover:
			return uiMix(cSurfaceAlt, cAccent, 0.10), cText, uiTint(cAccent, 0.42)
		case pressed:
			return cBorder, cText, cBorder
		default:
			return cSurfaceAlt, cTextMuted, cBorder
		}
	}
}

func (u *uiCanvas) paintButton(c *walk.Canvas, it uiItem, idx int) {
	r := it.rect
	hover := idx == u.hoverIdx
	pressed := idx == u.pressIdx
	focused := idx == u.focusIdx && u.focusVisible

	if focused {
		// Drawn outside the control, so showing focus never changes its size.
		uiFill(c, uiExpand(r, uiMetric(2)), uiMetric(10), uiTint(cAccent, 0.55))
	}

	fill, text, border := buttonSkin(it.variant, it.enabled, hover, pressed)
	uiPanel(c, r, uiMetric(8), fill, border)

	// Buttons carry text only: an icon would have to be optically centred
	// against the label, and the section headers already carry the glyphs.
	uiTextCenter(c, it.label, fButton, text, uiPadX(r, 8))
}

// Switch geometry: a 40x22 track with a 16 px thumb inset by 2, both scaled with
// the display. Fixed pixel sizes made the thumb read as a squeezed oval on a
// scaled display, because only the row it sat in grew while the track did not.
const (
	switchTrackW = 40
	switchTrackH = 22
	switchThumbD = 16
	switchInset  = 2
)

func (u *uiCanvas) paintSwitch(c *walk.Canvas, it uiItem, idx int) {
	trackW, trackH := uiMetric(switchTrackW), uiMetric(switchTrackH)
	thumbD, inset := uiMetric(switchThumbD), uiMetric(switchInset)
	ring := uiMetric(5)

	r := it.rect
	hover := idx == u.hoverIdx
	pressed := idx == u.pressIdx
	top := r.Y + (r.Height-trackH)/2

	if idx == u.focusIdx && u.focusVisible {
		// The ring wraps the toggle itself, not the whole row: the row is only
		// a layout slot, the switch is what a click operates.
		uiRoundFill(c, walk.Rectangle{
			X:      r.X - ring,
			Y:      top - ring,
			Width:  trackW + 2*ring,
			Height: trackH + 2*ring,
		}, (trackH+2*ring)/2, uiTint(cAccent, 0.55))
	}

	track, knob := cSurfaceAlt, cTextMuted
	switch {
	case !it.enabled:
		track, knob = cSurfaceAlt, cTextDim
	case it.checked:
		track, knob = cAccent, cWhite
	case hover:
		// A visible track change on hover, so the "off" state never reads as a
		// disabled control and the accent is reserved for "on".
		track, knob = uiMix(cSurfaceAlt, cAccent, 0.22), cText
	}
	if pressed && it.enabled {
		track = uiMix(track, cBackground, 0.18)
	}

	// A stadium rather than a rounded rectangle: a corner ellipse of half the track
	// height is what makes both ends fully round. Drawn through the alpha path, so
	// the 11 px caps come out as curves rather than as eleven steps.
	uiRoundFill(c, walk.Rectangle{X: r.X, Y: top, Width: trackW, Height: trackH}, trackH/2, track)

	// The thumb is a true circle sitting exactly `inset` from the end it is at, so
	// it can never be clipped by the track.
	knobX := r.X + inset
	if it.checked {
		knobX = r.X + trackW - thumbD - inset
	}
	uiRoundDot(c, knobX, top+(trackH-thumbD)/2, thumbD, knob)

	labelColor := cText
	if !it.enabled {
		labelColor = cTextDim
	}
	uiTextLeft(c, it.label, fBody, labelColor,
		walk.Rectangle{
			X:      r.X + trackW + uiMetric(12),
			Y:      r.Y,
			Width:  r.Width - trackW - uiMetric(12),
			Height: r.Height,
		})
}

func (u *uiCanvas) paintCaption(c *walk.Canvas, it uiItem) {
	font, col := uiFont(fBody), cText
	switch it.role {
	case capStrong:
		font = fButton
	case capMuted:
		font, col = fMuted, cTextMuted
	case capTiny:
		font, col = fTiny, cTextDim
	}
	if it.right {
		// The value slot keeps its own face and colour, so the hierarchy of the
		// playback row survives any file name length.
		uiTextRight(c, it.label, font, col, it.rect)
		return
	}
	r := uiIcon(c, it.icon, uiMix(col, cAccent, 0.35), it.rect, mIconTitle)
	uiTextLeft(c, it.label, font, col, r)
}

// paintScrollbar is the only scroll affordance; the canvas has no native one.
func (u *uiCanvas) paintScrollbar(c *walk.Canvas, size walk.Size) {
	maxScroll := u.content - u.viewH
	if maxScroll <= 0 {
		u.scrollbarHas = false
		return
	}
	barW := uiMetric(10)

	trackH := size.Height - 2*mPad - uiMetric(8)
	if trackH < uiMetric(40) {
		return
	}
	barH := int(float64(trackH) * float64(size.Height) / float64(u.content))
	barH = uiMinInt(uiMaxInt(uiMetric(28), barH), trackH)

	off := int(float64(trackH-barH) * float64(u.scroll) / float64(maxScroll))

	// Remember the geometry so onMouseDown/Move/Up can drag and click the bar.
	u.scrollbarX = size.Width - barW - uiMetric(6)
	u.scrollbarW = barW
	u.scrollbarTrackY = mPad + uiMetric(4)
	u.scrollbarTrackH = trackH
	u.scrollbarThumbY = mPad + uiMetric(4) + off
	u.scrollbarThumbH = barH
	u.scrollbarSize = size

	u.scrollbarHas = true

	// Track: a visible trough so the bar reads as a control, not a stray line.
	uiFill(c, walk.Rectangle{
		X:      u.scrollbarX,
		Y:      u.scrollbarTrackY,
		Width:  u.scrollbarW,
		Height: u.scrollbarTrackH,
	}, u.scrollbarW/2, uiMix(cSurfaceAlt, cTextMuted, 0.5))
	// Thumb: bright, clearly grabbable.
	uiFill(c, walk.Rectangle{
		X:      u.scrollbarX,
		Y:      u.scrollbarThumbY,
		Width:  u.scrollbarW,
		Height: u.scrollbarThumbH,
	}, u.scrollbarW/2, cTextMuted)
}
