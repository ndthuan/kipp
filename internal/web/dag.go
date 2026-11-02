package web

import (
	"github.com/ndthuan/kipp/internal/pipeline"
	"github.com/ndthuan/kipp/internal/store"
)

const (
	nodeW, nodeH = 160, 36
	gapX, gapY   = 60, 16
	labelRunes   = 18 // what fits a node at the sm size
)

// drawing is what the "dag" template draws: the stages in columns by
// depth, with a line from each need. The template writes the markup, so
// every name in it is escaped where it lands.
type drawing struct {
	Width, Height int
	Lines         []line
	Nodes         []node
}

type line struct{ X1, Y1, X2, Y2 int }

type node struct {
	Name, Label  string
	Status       store.StageStatus
	X, Y, W, H   int
	TextX, TextY int
}

// dagOf lays the stages out for the "dag" template; with none there is
// nothing to draw. The needs are the build's own; one that names no stage
// of the build draws no line.
func dagOf(stages []*store.Stage, needs map[string][]string) *drawing {
	if len(stages) == 0 {
		return nil
	}
	status := map[string]store.StageStatus{}
	for _, s := range stages {
		status[s.Name] = s.Status
	}
	graph := make(map[string][]string, len(stages))
	for _, s := range stages {
		graph[s.Name] = nil
		for _, n := range needs[s.Name] {
			if _, ok := status[n]; ok {
				graph[s.Name] = append(graph[s.Name], n)
			}
		}
	}
	dag, err := pipeline.New(graph, nil)
	if err != nil {
		return nil
	}
	at, width, height := layout(dag)
	d := &drawing{Width: width, Height: height}
	for _, name := range dag.Names() {
		for _, n := range graph[name] {
			from, to := at[n], at[name]
			d.Lines = append(d.Lines, line{from.x + nodeW, from.y + nodeH/2, to.x, to.y + nodeH/2})
		}
	}
	for _, name := range dag.Names() {
		p := at[name]
		d.Nodes = append(d.Nodes, node{Name: name, Label: label(name), Status: status[name], X: p.x, Y: p.y, W: nodeW, H: nodeH, TextX: p.x + 12, TextY: p.y + nodeH/2 + 5})
	}
	return d
}

type pos struct{ x, y int }

// layout puts each stage's node in the column of its depth, one under the
// other, and gives the size of the whole drawing.
func layout(dag *pipeline.DAG) (at map[string]pos, width, height int) {
	at = map[string]pos{}
	rows := map[int]int{}
	maxDepth, maxRow := 0, 0
	for _, name := range dag.Names() {
		d := dag.Depth(name)
		at[name] = pos{d * (nodeW + gapX), rows[d] * (nodeH + gapY)}
		rows[d]++
		maxDepth, maxRow = max(maxDepth, d), max(maxRow, rows[d])
	}
	return at, (maxDepth+1)*nodeW + maxDepth*gapX, maxRow*(nodeH+gapY) - gapY
}

// label is a stage's name cut to what fits its node; the title has all of it.
func label(name string) string {
	if r := []rune(name); len(r) > labelRunes+1 {
		return string(r[:labelRunes]) + "..."
	}
	return name
}
