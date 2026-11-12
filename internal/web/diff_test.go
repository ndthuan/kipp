package web

import (
	"strings"
	"testing"
)

// signs is a diff as its marks and texts, one line each.
func signs(lines []diffLine) string {
	var b strings.Builder
	for _, l := range lines {
		if l.Kind == diffGap {
			b.WriteString("~\n")
			continue
		}
		b.WriteString(l.Sign() + l.Text + "\n")
	}
	return b.String()
}

func TestLineDiffKeepsWhatBothShareAndMarksTheRest(t *testing.T) {
	got := signs(lineDiff("a\nb\nc\nd\n", "a\nc\nx\nd\n"))
	eq(t, "diff", got, " a\n-b\n c\n+x\n d\n")
	eq(t, "from nothing", signs(lineDiff("", "a\n")), "+a\n")
	eq(t, "to nothing", signs(lineDiff("a\nb", "")), "-a\n-b\n")
	eq(t, "the same", signs(lineDiff("a\n", "a")), " a\n")
}

func TestLineDiffNumbersEachSide(t *testing.T) {
	lines := lineDiff("a\nb\nc\n", "a\nx\nc\n")
	eq(t, "removed b", lines[1].Old, 2)
	eq(t, "removed b has no new line", lines[1].New, 0)
	eq(t, "added x", lines[2].New, 2)
	eq(t, "kept c old", lines[3].Old, 3)
	eq(t, "kept c new", lines[3].New, 3)
}

func TestADiffTooLargeToTableIsAllRemovedThenAllAdded(t *testing.T) {
	var a, b strings.Builder
	for i := range 2100 {
		a.WriteString("a" + strings.Repeat("x", i%7) + "\n")
		b.WriteString("b" + strings.Repeat("y", i%5) + "\n")
	}
	lines := lineDiff(a.String(), b.String())
	if len(lines) != 4200 || lines[0].Kind != diffDel || lines[2100].Kind != diffAdd {
		t.Fatalf("%d lines, first %+v", len(lines), lines[0])
	}
}

func TestHunksCutLongUnchangedRunsToTheContext(t *testing.T) {
	var a strings.Builder
	for i := range 20 {
		a.WriteString(string(rune('a'+i)) + "\n")
	}
	b := strings.Replace(a.String(), "k\n", "K\n", 1)
	d := diffOf(a.String(), b)
	eq(t, "hunks", signs(d.Lines), "~\n h\n i\n j\n-k\n+K\n l\n m\n n\n~\n")
	eq(t, "counts", d.Added+d.Taken, 2)
	eq(t, "gap size", d.Lines[0].N, 7)
	if d.Same() || !diffOf("a\n", "a\n").Same() {
		t.Fatal("Same is wrong")
	}
}

func TestSidesPutARemovedRunBesideTheRunAddedInItsPlace(t *testing.T) {
	rows := sides("a\nb\nc\nd\n", "a\nX\nd\ne\n")
	eq(t, "rows", len(rows), 5)
	eq(t, "kept", rows[0].Left.Text+rows[0].Right.Text, "aa")
	eq(t, "b beside X", rows[1].Left.Text+rows[1].Right.Text, "bX")
	eq(t, "c alone", rows[2].Left.Text+"|"+rows[2].Right.Kind, "c|")
	eq(t, "e alone", rows[4].Left.Kind+"|"+rows[4].Right.Text, "|e")
}
