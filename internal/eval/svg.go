package eval

import (
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// MaxSeries is the number of distinct series colours. A chart with more
// series is rejected instead of reusing a colour.
const MaxSeries = 8

// Categorical series colours in fixed order, with separate steps for light
// and dark backgrounds (validated for colour-vision deficiencies as
// adjacent pairs). Slots 3-5 are below 3:1 contrast on the light
// background, so publish each chart next to its numbers (for example the
// summary.md table) rather than on its own.
var (
	seriesLight = [MaxSeries]string{"#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"}
	seriesDark  = [MaxSeries]string{"#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9", "#e66767"}
)

// styleSheet themes every chart; the dark steps apply when the viewer
// prefers a dark colour scheme.
var styleSheet = buildStyleSheet()

func buildStyleSheet() string {
	var b strings.Builder
	b.WriteString("<style>\n")
	b.WriteString(`text{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;font-size:12px;fill:#52514e}` + "\n")
	b.WriteString(".title{font-size:14px;font-weight:600;fill:#0b0b0b}.bg{fill:#fcfcfb}\n")
	b.WriteString(".grid{stroke:#e1e0d9;stroke-width:1}.axis{stroke:#c3c2b7;stroke-width:1}.err{stroke:#52514e;stroke-width:1.5}\n")
	b.WriteString(".line{fill:none;stroke-width:2;stroke-linejoin:round;stroke-linecap:round}\n")
	for i, c := range seriesLight {
		fmt.Fprintf(&b, ".k%d{stroke:%s}.f%d{fill:%s}\n", i+1, c, i+1, c)
	}
	b.WriteString("@media (prefers-color-scheme:dark){\n")
	b.WriteString("text{fill:#c3c2b7}.title{fill:#f0efec}.bg{fill:#1a1a19}\n")
	b.WriteString(".grid{stroke:#2c2c2a}.axis{stroke:#383835}.err{stroke:#c3c2b7}\n")
	for i, c := range seriesDark {
		fmt.Fprintf(&b, ".k%d{stroke:%s}.f%d{fill:%s}\n", i+1, c, i+1, c)
	}
	b.WriteString("}\n</style>\n")
	return b.String()
}

const (
	defaultWidth  = 720
	defaultHeight = 360
	charWidth     = 7.0 // approximate advance of one 12px sans-serif glyph
	legendRow     = 20.0
	sidePad       = 16.0
	minPlotSide   = 40.0
)

// canvas accumulates one SVG document.
type canvas struct {
	b             strings.Builder
	width, height float64
	// next is the y coordinate below the title and legend.
	next float64
}

func newCanvas(width, height int, title string) *canvas {
	if width <= 0 {
		width = defaultWidth
	}
	if height <= 0 {
		height = defaultHeight
	}
	c := &canvas{width: float64(width), height: float64(height), next: sidePad}
	c.printf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`+"\n",
		width, height, width, height, esc(title))
	c.b.WriteString(styleSheet)
	c.printf(`<rect class="bg" width="%d" height="%d"/>`+"\n", width, height)
	if title != "" {
		c.printf(`<text class="title" x="%s" y="24">%s</text>`+"\n", f1(sidePad), esc(title))
		c.next = 36
	}
	return c
}

func (c *canvas) printf(format string, args ...any) { fmt.Fprintf(&c.b, format, args...) }

func (c *canvas) finish(w io.Writer) error {
	c.b.WriteString("</svg>\n")
	_, err := io.WriteString(w, c.b.String())
	return err
}

// legend lays out one entry per series in rows below the title. A single
// series gets no legend: the title names it.
func (c *canvas) legend(names []string, swatch func(slot int, x, y float64)) {
	if len(names) < 2 {
		return
	}
	x, y := sidePad, c.next+legendRow/2
	for i, name := range names {
		w := 22 + charWidth*float64(len([]rune(name))) + 16
		if x > sidePad && x+w > c.width-sidePad {
			x, y = sidePad, y+legendRow
		}
		swatch(i+1, x, y)
		c.printf(`<text x="%s" y="%s" dy=".32em">%s</text>`+"\n", f1(x+22), f1(y), esc(name))
		x += w
	}
	c.next = y + legendRow/2
}

// plotArea is the rectangle data is drawn in, with its data domain.
type plotArea struct {
	left, top, right, bottom float64
	xLo, xHi, yLo, yHi       float64
}

func (p plotArea) x(v float64) float64 {
	return p.left + (v-p.xLo)/(p.xHi-p.xLo)*(p.right-p.left)
}

func (p plotArea) y(v float64) float64 {
	return p.bottom - (v-p.yLo)/(p.yHi-p.yLo)*(p.bottom-p.top)
}

// plot places the plot area below the legend, leaving room for the y-axis
// labels on the left and bottomSpace for x labels.
func (c *canvas) plot(yLabel string, bottomSpace float64, yTicks []float64) (plotArea, error) {
	left := 56.0
	if yLabel != "" {
		left = 72
	}
	p := plotArea{left: left, top: c.next + 12, right: c.width - 24, bottom: c.height - bottomSpace,
		yLo: yTicks[0], yHi: yTicks[len(yTicks)-1]}
	if p.right-p.left < minPlotSide || p.bottom-p.top < minPlotSide {
		return p, fmt.Errorf("chart is too small (%gx%g) for its title, legend and axes", c.width, c.height)
	}
	return p, nil
}

// yAxis draws horizontal gridlines with tick labels and the y-axis label.
func (c *canvas) yAxis(p plotArea, ticks []float64, step float64, label string) {
	for _, t := range ticks {
		y := f1(p.y(t))
		c.printf(`<line class="grid" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", f1(p.left), y, f1(p.right), y)
		c.printf(`<text x="%s" y="%s" dy=".32em" text-anchor="end">%s</text>`+"\n", f1(p.left-8), y, tickLabel(t, step))
	}
	if label != "" {
		c.printf(`<text transform="translate(18 %s) rotate(-90)" text-anchor="middle" dy=".32em">%s</text>`+"\n",
			f1((p.top+p.bottom)/2), esc(label))
	}
}

// maxTickIndex bounds |lo/step| and |hi/step| in niceTicks: up to here
// consecutive tick indices are exact float64 integers.
const maxTickIndex = 1 << 52

// niceTicks returns evenly spaced round tick values (steps of 1, 2 or 5
// times a power of ten) covering [lo, hi] in about target intervals. When
// round ticks cannot be represented (the range is tiny relative to its
// magnitude, as with Unix-nanosecond x values, or the span overflows) it
// returns just the two ends, with step hi-lo.
func niceTicks(lo, hi float64, target int) (ticks []float64, step float64) {
	target = max(target, 1)
	if !(hi > lo) {
		hi = lo + 1
		if !(hi > lo) { // lo+1 rounds back to lo beyond 2^53
			hi = math.Nextafter(lo, math.Inf(1))
		}
	}
	raw := (hi - lo) / float64(target)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	switch f := raw / mag; {
	case f <= 1:
		step = mag
	case f <= 2:
		step = 2 * mag
	case f <= 5:
		step = 5 * mag
	default:
		step = 10 * mag
	}
	kLo, kHi := math.Floor(lo/step+1e-9), math.Ceil(hi/step-1e-9)
	// A regular result has at most target+2 intervals; anything else (NaN
	// included) means the indices are not exact and stepping them could
	// stall, so fall back to the bare range.
	if math.IsInf(step, 0) || !(kHi-kLo >= 0 && kHi-kLo <= float64(4*target)) ||
		math.Abs(kLo) > maxTickIndex || math.Abs(kHi) > maxTickIndex {
		return []float64{lo, hi}, hi - lo
	}
	for i := 0; i <= int(kHi-kLo); i++ {
		ticks = append(ticks, (kLo+float64(i))*step)
	}
	return ticks, step
}

// tickLabel formats a tick with as many decimals as the step needs,
// compacting thousands as "2K".
func tickLabel(v, step float64) string {
	if math.Abs(v) < step*1e-6 {
		return "0"
	}
	if step >= 1000 {
		return strconv.FormatFloat(v/1000, 'f', -1, 64) + "K"
	}
	decimals := max(0, int(-math.Floor(math.Log10(step)+1e-9)))
	return strconv.FormatFloat(v, 'f', decimals, 64)
}

// span tracks the range of the finite values added to it.
type span struct {
	lo, hi float64
	ok     bool
}

func (s *span) add(v float64) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	if !s.ok {
		s.lo, s.hi, s.ok = v, v, true
		return
	}
	s.lo, s.hi = min(s.lo, v), max(s.hi, v)
}

// wrapLabel splits s into lines of at most maxChars runes. It breaks at
// spaces, then after the hyphens of a word that is too long for one line,
// and shortens a piece that still does not fit with an ellipsis, reporting
// that in truncated so the caller can attach the full text as a tooltip.
func wrapLabel(s string, maxChars int) (lines []string, truncated bool) {
	maxChars = max(maxChars, 2)
	cur := ""
	for _, word := range strings.Fields(s) {
		for i, piece := range splitWord(word, maxChars) {
			if r := []rune(piece); len(r) > maxChars {
				piece, truncated = string(r[:maxChars-1])+"…", true
			}
			sep := " "
			if i > 0 {
				sep = "" // the pieces of one hyphenated word join directly
			}
			switch {
			case cur == "":
				cur = piece
			case runeLen(cur)+len(sep)+runeLen(piece) <= maxChars:
				cur += sep + piece
			default:
				lines = append(lines, cur)
				cur = piece
			}
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines, truncated
}

// splitWord returns word whole when it fits in maxChars runes, and
// otherwise its pieces split after each hyphen ("a-b" gives "a-", "b").
func splitWord(word string, maxChars int) []string {
	if runeLen(word) <= maxChars {
		return []string{word}
	}
	var pieces []string
	for _, p := range strings.SplitAfter(word, "-") {
		if p != "" {
			pieces = append(pieces, p)
		}
	}
	return pieces
}

func runeLen(s string) int { return len([]rune(s)) }

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

func esc(s string) string { return xmlEscaper.Replace(s) }

// f1 formats a coordinate with one decimal.
func f1(v float64) string { return strconv.FormatFloat(v, 'f', 1, 64) }
