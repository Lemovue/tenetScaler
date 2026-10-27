package eval

import (
	"fmt"
	"io"
	"math"
	"strings"
)

// Series is one line of a LineChart. A point with a NaN or infinite
// coordinate breaks the line.
type Series struct {
	Name string
	X, Y []float64
}

// LineChart plots one or more series against a shared x axis, typically a
// time series from a run's timeline. The y axis always includes zero.
// Titles of charts showing simulated numbers should say so.
type LineChart struct {
	Title, XLabel, YLabel string
	Series                []Series
	// Width and Height are in pixels; zero means 720x360.
	Width, Height int
}

// WriteSVG renders the chart as a standalone SVG document that follows the
// viewer's light or dark colour scheme.
func (c LineChart) WriteSVG(w io.Writer) error {
	if err := checkSeriesCount("line", c.Title, len(c.Series)); err != nil {
		return err
	}
	var xs, ys span
	names := make([]string, len(c.Series))
	for i, s := range c.Series {
		if len(s.X) != len(s.Y) {
			return fmt.Errorf("line chart %q: series %q has %d x values and %d y values", c.Title, s.Name, len(s.X), len(s.Y))
		}
		for j := range s.X {
			if finite(s.X[j]) && finite(s.Y[j]) {
				xs.add(s.X[j])
				ys.add(s.Y[j])
			}
		}
		names[i] = s.Name
	}
	cv := newCanvas(c.Width, c.Height, c.Title)
	cv.legend(names, func(slot int, x, y float64) {
		cv.printf(`<line class="line k%d" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", slot, f1(x), f1(y), f1(x+16), f1(y))
	})
	yTicks, yStep := niceTicks(min(0, ys.lo), max(0, ys.hi), 5)
	xTicks, xStep := niceTicks(xs.lo, xs.hi, 6)
	p, err := cv.plot(c.YLabel, 48, yTicks)
	if err != nil {
		return fmt.Errorf("line chart %q: %w", c.Title, err)
	}
	p.xLo, p.xHi = xTicks[0], xTicks[len(xTicks)-1]
	cv.yAxis(p, yTicks, yStep, c.YLabel)
	cv.printf(`<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", f1(p.left), f1(p.bottom), f1(p.right), f1(p.bottom))
	for _, t := range xTicks {
		cv.printf(`<text x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", f1(p.x(t)), f1(p.bottom+18), tickLabel(t, xStep))
	}
	if c.XLabel != "" {
		cv.printf(`<text x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", f1((p.left+p.right)/2), f1(p.bottom+38), esc(c.XLabel))
	}
	for i, s := range c.Series {
		cv.printf(`<path class="line k%d" d="%s"><title>%s</title></path>`+"\n", i+1, linePath(p, s), esc(s.Name))
	}
	return cv.finish(w)
}

// linePath builds the path data of a series, starting a new subpath after
// every missing point.
func linePath(p plotArea, s Series) string {
	var b strings.Builder
	cmd := "M"
	for i := range s.X {
		if !finite(s.X[i]) || !finite(s.Y[i]) {
			cmd = "M"
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(cmd + f1(p.x(s.X[i])) + " " + f1(p.y(s.Y[i])))
		cmd = "L"
	}
	return b.String()
}

// BarSeries is one bar per group, for example one tenant across setups.
// Errors are symmetric error-bar half-widths such as Stat.CI95 and may be
// nil. A NaN value draws no bar; a NaN error draws no error bar.
type BarSeries struct {
	Name           string
	Values, Errors []float64
}

// BarChart is a grouped bar chart with optional error bars, for example
// the mean ± 95% CI of a metric per setup. The y axis always includes zero.
type BarChart struct {
	Title, YLabel string
	// Groups are the x-axis categories; each series has one value per group.
	Groups []string
	Series []BarSeries
	// Width and Height are in pixels; zero means 720x360.
	Width, Height int
}

// Bar geometry: bars are at most 24 px wide, separated by a 2 px gap, with
// a 4 px rounded end away from the baseline.
const (
	maxBarWidth = 24.0
	barGap      = 2.0
	barRadius   = 4.0
)

// WriteSVG renders the chart as a standalone SVG document that follows the
// viewer's light or dark colour scheme.
func (c BarChart) WriteSVG(w io.Writer) error {
	if len(c.Groups) == 0 {
		return fmt.Errorf("bar chart %q has no groups", c.Title)
	}
	if err := checkSeriesCount("bar", c.Title, len(c.Series)); err != nil {
		return err
	}
	var ys span
	names := make([]string, len(c.Series))
	for i, s := range c.Series {
		if len(s.Values) != len(c.Groups) || (s.Errors != nil && len(s.Errors) != len(c.Groups)) {
			return fmt.Errorf("bar chart %q: series %q needs %d values (and errors, if any), got %d and %d",
				c.Title, s.Name, len(c.Groups), len(s.Values), len(s.Errors))
		}
		for j, v := range s.Values {
			e := errorAt(s, j)
			if e < 0 {
				return fmt.Errorf("bar chart %q: series %q has a negative error %v", c.Title, s.Name, e)
			}
			ys.add(v)
			if finite(v) && finite(e) {
				ys.add(v - e)
				ys.add(v + e)
			}
		}
		names[i] = s.Name
	}
	cv := newCanvas(c.Width, c.Height, c.Title)
	cv.legend(names, func(slot int, x, y float64) {
		cv.printf(`<rect class="f%d" x="%s" y="%s" width="10" height="10" rx="2"/>`+"\n", slot, f1(x+3), f1(y-5))
	})
	band := (cv.width - 96) / float64(len(c.Groups))
	maxChars := max(4, int(band/charWidth))
	labelLines := 1
	for _, g := range c.Groups {
		lines, _ := wrapLabel(g, maxChars)
		labelLines = max(labelLines, len(lines))
	}
	yTicks, yStep := niceTicks(min(0, ys.lo), max(0, ys.hi), 5)
	p, err := cv.plot(c.YLabel, 22+14*float64(labelLines), yTicks)
	if err != nil {
		return fmt.Errorf("bar chart %q: %w", c.Title, err)
	}
	cv.yAxis(p, yTicks, yStep, c.YLabel)
	band = (p.right - p.left) / float64(len(c.Groups))
	n := float64(len(c.Series))
	barW := max(1, min(maxBarWidth, (band*0.7-barGap*(n-1))/n))
	base := p.y(0)
	for g, group := range c.Groups {
		x0 := p.left + float64(g)*band + (band-(n*barW+barGap*(n-1)))/2
		for i, s := range c.Series {
			x := x0 + float64(i)*(barW+barGap)
			cv.bar(p, x, barW, base, s, g, group, i+1)
		}
		cv.groupLabel(group, maxChars, p.left+(float64(g)+0.5)*band, p.bottom+18)
	}
	cv.printf(`<line class="axis" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", f1(p.left), f1(base), f1(p.right), f1(base))
	return cv.finish(w)
}

// bar draws one bar (with its tooltip) and its error bar.
func (c *canvas) bar(p plotArea, x, width, base float64, s BarSeries, g int, group string, slot int) {
	v, e := s.Values[g], errorAt(s, g)
	if !finite(v) {
		return
	}
	tip := fmt.Sprintf("%s, %s: %s", s.Name, group, FormatValue(v))
	if finite(e) {
		tip += " ± " + FormatValue(e)
	}
	if top := p.y(v); top != base {
		c.printf(`<path class="f%d" d="%s"><title>%s</title></path>`+"\n", slot, barPath(x, width, base, top), esc(tip))
	}
	if finite(e) && e > 0 {
		cx, half := x+width/2, min(8, width)/2
		lo, hi := f1(p.y(v-e)), f1(p.y(v+e))
		c.printf(`<line class="err" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", f1(cx), lo, f1(cx), hi)
		for _, y := range []string{lo, hi} {
			c.printf(`<line class="err" x1="%s" y1="%s" x2="%s" y2="%s"/>`+"\n", f1(cx-half), y, f1(cx+half), y)
		}
	}
}

// groupLabel writes a wrapped x-axis category label centred on x, first
// line at y. A label shortened to fit carries its full text as a tooltip.
func (c *canvas) groupLabel(group string, maxChars int, x, y float64) {
	lines, truncated := wrapLabel(group, maxChars)
	if truncated {
		c.printf("<g><title>%s</title>\n", esc(group))
	}
	for k, line := range lines {
		c.printf(`<text x="%s" y="%s" text-anchor="middle">%s</text>`+"\n", f1(x), f1(y+14*float64(k)), esc(line))
	}
	if truncated {
		c.b.WriteString("</g>\n")
	}
}

// barPath outlines a bar from the baseline to top: square at the baseline,
// rounded at the data end (which is below the baseline for negative values).
func barPath(x, width, base, top float64) string {
	r := min(barRadius, width/2, math.Abs(top-base))
	dir := 1.0 // the rounded corners curve downwards from a top above base
	if top > base {
		dir = -1
	}
	return fmt.Sprintf("M%s %s V%s Q%s %s %s %s H%s Q%s %s %s %s V%s Z",
		f1(x), f1(base), f1(top+dir*r),
		f1(x), f1(top), f1(x+r), f1(top),
		f1(x+width-r),
		f1(x+width), f1(top), f1(x+width), f1(top+dir*r),
		f1(base))
}

func errorAt(s BarSeries, i int) float64 {
	if s.Errors == nil {
		return math.NaN()
	}
	return s.Errors[i]
}

func checkSeriesCount(kind, title string, n int) error {
	switch {
	case n == 0:
		return fmt.Errorf("%s chart %q has no series", kind, title)
	case n > MaxSeries:
		return fmt.Errorf("%s chart %q has %d series; at most %d have distinct colours (fold the rest or split the chart)",
			kind, title, n, MaxSeries)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
