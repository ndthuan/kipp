package web

import (
	"slices"
	"strconv"
	"strings"
)

const (
	diffSame = "same"
	diffAdd  = "add"
	diffDel  = "del"
	diffGap  = "gap"
)

const diffContext = 3

const maxDiffCells = 4_000_000

type diffLine struct {
	Kind     string
	Old, New int
	Text     string
	N        int
}

// Sign is the mark a line is shown with.
func (l diffLine) Sign() string {
	switch l.Kind {
	case diffAdd:
		return "+"
	case diffDel:
		return "-"
	}
	return " "
}

func splitLines(text string) []string {
	if text == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(text, "\n"), "\n")
}

func lineDiff(a, b string) []diffLine {
	x, y := splitLines(a), splitLines(b)
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	out := make([]diffLine, 0, len(x)+len(y))
	for i := range pre {
		out = append(out, diffLine{Kind: diffSame, Old: i + 1, New: i + 1, Text: x[i]})
	}
	out = append(out, middle(x[pre:len(x)-suf], y[pre:len(y)-suf], pre)...)
	for i := range suf {
		oi, ni := len(x)-suf+i, len(y)-suf+i
		out = append(out, diffLine{Kind: diffSame, Old: oi + 1, New: ni + 1, Text: x[oi]})
	}
	return out
}

func middle(x, y []string, skip int) []diffLine {
	if len(x)*len(y) > maxDiffCells {
		return replaced(x, y, skip)
	}
	lcs := lcsTable(x, y)
	var out []diffLine
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out = append(out, diffLine{Kind: diffSame, Old: skip + i + 1, New: skip + j + 1, Text: x[i]})
			i++
			j++
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			out = append(out, diffLine{Kind: diffDel, Old: skip + i + 1, Text: x[i]})
			i++
		default:
			out = append(out, diffLine{Kind: diffAdd, New: skip + j + 1, Text: y[j]})
			j++
		}
	}
	return out
}

func replaced(x, y []string, skip int) []diffLine {
	out := make([]diffLine, 0, len(x)+len(y))
	for i, s := range x {
		out = append(out, diffLine{Kind: diffDel, Old: skip + i + 1, Text: s})
	}
	for j, s := range y {
		out = append(out, diffLine{Kind: diffAdd, New: skip + j + 1, Text: s})
	}
	return out
}

func lcsTable(x, y []string) [][]int32 {
	lcs := make([][]int32, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int32, len(y)+1)
	}
	for i, xi := range slices.Backward(x) {
		for j, yj := range slices.Backward(y) {
			if xi == yj {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	return lcs
}

func hunks(lines []diffLine) []diffLine {
	var out []diffLine
	for i := 0; i < len(lines); {
		if lines[i].Kind != diffSame {
			out = append(out, lines[i])
			i++
			continue
		}
		end := i
		for end < len(lines) && lines[end].Kind == diffSame {
			end++
		}
		keepHead, keepTail := diffContext, diffContext
		if i == 0 {
			keepHead = 0
		}
		if end == len(lines) {
			keepTail = 0
		}
		if end-i <= keepHead+keepTail {
			out = append(out, lines[i:end]...)
		} else {
			out = append(out, lines[i:i+keepHead]...)
			out = append(out, diffLine{Kind: diffGap, N: end - i - keepHead - keepTail})
			out = append(out, lines[end-keepTail:end]...)
		}
		i = end
	}
	return out
}

type diffView struct {
	Lines        []diffLine
	Added, Taken int
}

// Same says the two texts have the same lines.
func (d diffView) Same() bool { return d.Added == 0 && d.Taken == 0 }

func diffOf(a, b string) diffView {
	all := lineDiff(a, b)
	d := diffView{}
	for _, l := range all {
		switch l.Kind {
		case diffAdd:
			d.Added++
		case diffDel:
			d.Taken++
		}
	}
	d.Lines = hunks(all)
	return d
}

type sideRow struct {
	Left, Right diffLine
}

type sidesView struct {
	Left, Right string
	Rows        []sideRow
}

func lineNo(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

func sides(left, right string) []sideRow {
	lines := lineDiff(left, right)
	var rows []sideRow
	for i := 0; i < len(lines); {
		if lines[i].Kind == diffSame {
			rows = append(rows, sideRow{Left: lines[i], Right: lines[i]})
			i++
			continue
		}
		del := runOf(lines[i:], diffDel)
		i += len(del)
		add := runOf(lines[i:], diffAdd)
		i += len(add)
		rows = append(rows, paired(del, add)...)
	}
	return rows
}

func runOf(lines []diffLine, kind string) []diffLine {
	n := 0
	for n < len(lines) && lines[n].Kind == kind {
		n++
	}
	return lines[:n]
}

func paired(del, add []diffLine) []sideRow {
	rows := make([]sideRow, max(len(del), len(add)))
	for k := range rows {
		if k < len(del) {
			rows[k].Left = del[k]
		}
		if k < len(add) {
			rows[k].Right = add[k]
		}
	}
	return rows
}
