package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ndthuan/kipp/internal/overview"
)

// The graphs of the front page are drawn here, on the server, as inline
// SVG: a page reads without a script, and the refresh that redraws the page
// redraws them. Every graph comes with its numbers as a table that only a
// screen reader sees, and as its <desc>.

// px is a coordinate of a graph, printed to a tenth.
type px float64

func (p px) String() string { return strconv.FormatFloat(float64(p), 'f', 1, 64) }

type grect struct {
	X, Y, W, H px
	Class      string
	// Title is what a pointer over the mark shows.
	Title string
}

type gline struct {
	X1, Y1, X2, Y2 px
	Class          string
}

type glabel struct {
	X, Y   px
	Text   string
	Anchor string // start, middle or end
	Class  string
	Title  string
}

type legendItem struct{ Class, Text string }

// chart is a graph: its heading, the SVG's marks, the legend under it and
// the numbers behind it.
type chart struct {
	Key, Heading, Title, Desc string
	W, H                      int
	Rects                     []grect
	Lines                     []gline
	Labels                    []glabel
	Legend                    []legendItem
	Head                      []string
	Rows                      [][]string
}

const (
	chartW    = 400
	chartLeft = 38
	chartPad  = 6
	labelGap  = 12
)

// chartsOf is the three graphs of c, with the times in zone.
func chartsOf(c *overview.Charts, zone *time.Location) []*chart {
	return []*chart{buildsChart(c, zone), slotsChart(c, zone), ratesChart(c)}
}

// clockLabel is a column's start as a tooltip and a table row give it.
func clockLabel(t time.Time, zone *time.Location) string { return t.In(zone).Format("Mon 15:04") }

// axisTicks are the labels under a graph's columns: every sixth hour of a
// day's graph and every midnight of a week's.
func axisTicks(c *overview.Charts, zone *time.Location, x0, slot px, y px) []glabel {
	var out []glabel
	for i, b := range c.Buckets {
		t := b.Start.In(zone)
		text := ""
		switch {
		case c.Range.Bucket == time.Hour && t.Hour()%6 == 0:
			text = t.Format("15:04")
		case c.Range.Bucket > time.Hour && t.Hour() == 0:
			text = t.Format("Mon 2")
		}
		if text != "" {
			out = append(out, glabel{X: x0 + px(i)*slot, Y: y, Text: text, Anchor: "start"})
		}
	}
	return out
}

// niceMax is the least of 1, 2 and 5 times a power of ten that is not below
// v, at least 1: the top of a count's axis.
func niceMax(v int) int {
	for step := 1; ; step *= 10 {
		for _, m := range []int{1, 2, 5} {
			if m*step >= v {
				return m * step
			}
		}
	}
}

// niceWait is the least of a few round times that is not below d.
func niceWait(d time.Duration) time.Duration {
	steps := []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute,
		30 * time.Minute, time.Hour, 2 * time.Hour, 6 * time.Hour, 24 * time.Hour}
	for _, s := range steps {
		if s >= d {
			return s
		}
	}
	return d
}

// shortWait is a duration as a graph's axis and table give it: 40s, 5m, 2h.
func shortWait(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return strconv.Itoa(int(d/time.Second)) + "s"
}

// yTicks puts gridlines and labels at 0, the middle when it is a whole
// number, and the top of a plot whose box is [top, top+h).
func (ch *chart) yTicks(top, h px, max int, text func(int) string) {
	ticks := []int{0, max}
	if max%2 == 0 && max > 2 {
		ticks = []int{0, max / 2, max}
	}
	for _, v := range ticks {
		y := top + h - px(v)*h/px(max)
		ch.Lines = append(ch.Lines, gline{X1: chartLeft, Y1: y, X2: chartW - chartPad, Y2: y, Class: "grid"})
		ch.Labels = append(ch.Labels, glabel{X: chartLeft - 4, Y: y + 4, Text: text(v), Anchor: "end"})
	}
}

func buildsChart(c *overview.Charts, zone *time.Location) *chart {
	var total [4]int
	most := 0
	for _, b := range c.Buckets {
		total[0], total[1], total[2], total[3] = total[0]+b.Success, total[1]+b.Unstable, total[2]+b.Failed, total[3]+b.Cancelled
		most = max(most, b.Finished())
	}
	ch := &chart{Key: "builds", Heading: "Builds over time", W: chartW, H: 176,
		Title: "Builds over time: how the builds that ended in the last " + c.Range.Label + " ended",
		Desc: fmt.Sprintf("In the last %s, %d builds ended success, %d unstable, %d failed and %d cancelled. Each column is %s.",
			c.Range.Label, total[0], total[1], total[2], total[3], columnWidth(c)),
		Head: []string{"From", "Success", "Unstable", "Failed", "Cancelled"}}
	ch.Legend = []legendItem{{"success", fmt.Sprint("success ", total[0])}, {"unstable", fmt.Sprint("unstable ", total[1])},
		{"failed", fmt.Sprint("failed ", total[2])}, {"cancelled", fmt.Sprint("cancelled ", total[3])}}
	top, h := px(8), px(140)
	top2 := niceMax(most)
	ch.yTicks(top, h, top2, strconv.Itoa)
	slot := px(chartW-chartLeft-chartPad) / px(len(c.Buckets))
	for i, b := range c.Buckets {
		name := clockLabel(b.Start, zone)
		ch.Rows = append(ch.Rows, []string{name, strconv.Itoa(b.Success), strconv.Itoa(b.Unstable), strconv.Itoa(b.Failed), strconv.Itoa(b.Cancelled)})
		tip := fmt.Sprintf("%s: %d success, %d unstable, %d failed, %d cancelled", name, b.Success, b.Unstable, b.Failed, b.Cancelled)
		x, y := chartLeft+px(i)*slot+slot*0.14, top+h
		for _, seg := range []struct {
			n     int
			class string
		}{{b.Success, "success"}, {b.Unstable, "unstable"}, {b.Failed, "failed"}, {b.Cancelled, "cancelled"}} {
			if seg.n == 0 {
				continue
			}
			sh := px(seg.n) * h / px(top2)
			y -= sh
			ch.Rects = append(ch.Rects, grect{X: x, Y: y, W: slot * 0.72, H: sh, Class: "bar bar-" + seg.class, Title: tip})
		}
	}
	ch.Labels = append(ch.Labels, axisTicks(c, zone, chartLeft, slot, top+h+labelGap+4)...)
	return ch
}

func columnWidth(c *overview.Charts) string {
	return strings.TrimSuffix(c.Range.Bucket.String(), "m0s")
}

func slotsChart(c *overview.Charts, zone *time.Location) *chart {
	peak, longest := 0, time.Duration(0)
	for _, b := range c.Buckets {
		peak = max(peak, b.MaxRunning)
		longest = max(longest, b.WaitMax)
	}
	limit := "no limit"
	if c.BuildsLimit > 0 {
		limit = "the limit is " + strconv.Itoa(c.BuildsLimit)
	}
	ch := &chart{Key: "slots", Heading: "Slots and queue", W: chartW, H: 214,
		Title: "Slots and queue: builds running at once, and how long builds waited, in the last " + c.Range.Label,
		Desc: fmt.Sprintf("In the last %s at most %d builds ran at once (%s) and the longest a build waited to start was %s.",
			c.Range.Label, peak, limit, shortWait(longest)),
		Head: []string{"From", "Most running", "Started", "Median wait", "Longest wait"}}
	ch.Legend = []legendItem{{"running", "running at once, at most " + strconv.Itoa(peak)}}
	if c.BuildsLimit > 0 {
		ch.Legend = append(ch.Legend, legendItem{"limit", "limit " + strconv.Itoa(c.BuildsLimit)})
	}
	ch.Legend = append(ch.Legend, legendItem{"wait", "median wait"}, legendItem{"waitmax", "longest wait " + shortWait(longest)})

	slot := px(chartW-chartLeft-chartPad) / px(len(c.Buckets))
	topA, hA := px(18), px(92)
	runMax := niceMax(max(peak, c.BuildsLimit))
	ch.Labels = append(ch.Labels, glabel{X: chartLeft, Y: 10, Text: "Builds running at once", Anchor: "start", Class: "caption"})
	ch.yTicks(topA, hA, runMax, strconv.Itoa)
	topB, hB := px(138), px(48)
	waitMax := niceWait(longest)
	ch.Labels = append(ch.Labels, glabel{X: chartLeft, Y: 130, Text: "Wait in the queue", Anchor: "start", Class: "caption"})
	for _, v := range []time.Duration{0, waitMax} {
		y := topB + hB - px(v)*hB/px(waitMax)
		ch.Lines = append(ch.Lines, gline{X1: chartLeft, Y1: y, X2: chartW - chartPad, Y2: y, Class: "grid"})
		ch.Labels = append(ch.Labels, glabel{X: chartLeft - 4, Y: y + 4, Text: shortWait(v), Anchor: "end"})
	}
	for i, b := range c.Buckets {
		name := clockLabel(b.Start, zone)
		ch.Rows = append(ch.Rows, []string{name, strconv.Itoa(b.MaxRunning), strconv.Itoa(b.Started), shortWait(b.WaitMedian), shortWait(b.WaitMax)})
		tip := fmt.Sprintf("%s: at most %d running; %d started, waited %s (median) and %s (longest)", name, b.MaxRunning, b.Started, shortWait(b.WaitMedian), shortWait(b.WaitMax))
		x := chartLeft + px(i)*slot + slot*0.14
		if b.MaxRunning > 0 {
			rh := px(b.MaxRunning) * hA / px(runMax)
			ch.Rects = append(ch.Rects, grect{X: x, Y: topA + hA - rh, W: slot * 0.72, H: rh, Class: "bar bar-running", Title: tip})
		}
		if b.WaitMax > 0 {
			mh := max(px(b.WaitMax)*hB/px(waitMax), 1)
			ch.Rects = append(ch.Rects, grect{X: x, Y: topB + hB - mh, W: slot * 0.72, H: mh, Class: "bar bar-waitmax", Title: tip})
			mh = max(px(b.WaitMedian)*hB/px(waitMax), 1)
			ch.Rects = append(ch.Rects, grect{X: x, Y: topB + hB - mh, W: slot * 0.72, H: mh, Class: "bar bar-wait", Title: tip})
		}
	}
	if c.BuildsLimit > 0 {
		y := topA + hA - px(c.BuildsLimit)*hA/px(runMax)
		ch.Lines = append(ch.Lines, gline{X1: chartLeft, Y1: y, X2: chartW - chartPad, Y2: y, Class: "limit"})
	}
	ch.Labels = append(ch.Labels, axisTicks(c, zone, chartLeft, slot, topB+hB+labelGap+4)...)
	return ch
}

// rateRow is the height of a group's bar and the room its name has.
const (
	rateRow   = 24
	rateName  = 92
	rateBar   = 150
	rateNameN = 14
)

func ratesChart(c *overview.Charts) *chart {
	ch := &chart{Key: "rates", Heading: "Pass rate by group", W: chartW, H: rateRow*len(c.Groups) + 4,
		Title: "Pass rate by group: how many of the builds that ended in the last " + c.Range.Label + " ended green",
		Head:  []string{"Group", "Pass rate", "Builds", "Change", "Before"}}
	ch.Legend = []legendItem{{"success", "green"}, {"failed", "red"}}
	var parts []string
	for i, g := range c.Groups {
		name := groupName(g.Name)
		y := px(2 + i*rateRow)
		text, change := rateText(g)
		before := "no builds"
		if g.Before.Total() > 0 {
			before = fmt.Sprintf("%d%% of %d", g.Before.Percent(), g.Before.Total())
		}
		ch.Rows = append(ch.Rows, []string{name, rateWords(g), strconv.Itoa(g.Now.Total()), change, before})
		parts = append(parts, name+" "+rateWords(g))
		short := name
		if r := []rune(name); len(r) > rateNameN {
			short = string(r[:rateNameN-1]) + "..."
		}
		ch.Labels = append(ch.Labels,
			glabel{X: 0, Y: y + 15, Text: short, Anchor: "start", Title: name},
			glabel{X: rateName + rateBar + 8, Y: y + 15, Text: text, Anchor: "start"})
		tip := name + ": " + rateWords(g)
		ch.Rects = append(ch.Rects, grect{X: rateName, Y: y + 4, W: rateBar, H: 14, Class: "bar bar-track", Title: tip})
		if t := g.Now.Total(); t > 0 {
			gw := px(g.Now.Green) * rateBar / px(t)
			ch.Rects = append(ch.Rects, grect{X: rateName, Y: y + 4, W: gw, H: 14, Class: "bar bar-success", Title: tip},
				grect{X: rateName + gw, Y: y + 4, W: rateBar - gw, H: 14, Class: "bar bar-failed", Title: tip})
		}
	}
	ch.Desc = "In the last " + c.Range.Label + ", " + strings.Join(parts, "; ") + "."
	if len(parts) == 0 {
		ch.Desc = "No build ended in the last " + c.Range.Label + " or in the " + c.Range.Label + " before."
	}
	return ch
}

func groupName(n string) string {
	if n == "" {
		return "No group"
	}
	return n
}

// rateText is what stands beside a group's bar, "92% (+4) of 130", and the
// change on its own, for the table.
func rateText(g overview.GroupRate) (text, change string) {
	d, ok := g.Delta()
	change = "none"
	if ok {
		change = fmt.Sprintf("%+d", d)
	}
	if g.Now.Total() == 0 {
		return fmt.Sprintf("none (was %d%%)", g.Before.Percent()), change
	}
	if ok {
		return fmt.Sprintf("%d%% (%s) of %d", g.Now.Percent(), change, g.Now.Total()), change
	}
	return fmt.Sprintf("%d%% of %d", g.Now.Percent(), g.Now.Total()), change
}

// rateWords is the same for a screen reader.
func rateWords(g overview.GroupRate) string {
	text, _ := rateText(g)
	return text
}
