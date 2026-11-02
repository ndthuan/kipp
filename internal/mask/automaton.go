package mask

import "slices"

// automaton finds every one of many secrets in a stream in one pass
// (Aho-Corasick). Its nodes are numbered from the root, 0; a node's edges
// are a list through next.
type automaton struct {
	nodes, nedges int32
	first         []int32 // a node's first edge, or -1
	edges         []edge
	fail          []int32 // the longest proper suffix of the node's text that is a node
	depth         []int32
	dict          []int32 // the nearest node on the fail chain that ends a secret, or -1
	end           []bool
}

type edge struct {
	b    byte
	to   int32
	next int32
}

func newAutomaton(secrets [][]byte) *automaton {
	a := &automaton{}
	a.addNode(0)
	for _, s := range secrets {
		at := int32(0)
		for _, b := range s {
			to := a.child(at, b)
			if to < 0 {
				to = a.addNode(a.depth[at] + 1)
				a.edges = append(a.edges, edge{b: b, to: to, next: a.first[at]})
				a.first[at] = a.nedges
				a.nedges++
			}
			at = to
		}
		a.end[at] = true
	}
	a.link()
	return a
}

func (a *automaton) addNode(depth int32) int32 {
	a.first = append(a.first, -1)
	a.fail = append(a.fail, 0)
	a.depth = append(a.depth, depth)
	a.dict = append(a.dict, -1)
	a.end = append(a.end, false)
	a.nodes++
	return a.nodes - 1
}

// child is the node b leads to from at, or -1.
func (a *automaton) child(at int32, b byte) int32 {
	for e := a.first[at]; e >= 0; e = a.edges[e].next {
		if a.edges[e].b == b {
			return a.edges[e].to
		}
	}
	return -1
}

// link sets the fail and dict links breadth first.
func (a *automaton) link() {
	queue := []int32{0}
	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for e := a.first[u]; e >= 0; e = a.edges[e].next {
			v := a.edges[e].to
			if u != 0 {
				a.fail[v] = a.failOf(u, a.edges[e].b)
			}
			if f := a.fail[v]; a.end[f] {
				a.dict[v] = f
			} else {
				a.dict[v] = a.dict[f]
			}
			queue = append(queue, v)
		}
	}
}

// failOf is the fail link of the child of u that b leads to.
func (a *automaton) failOf(u int32, b byte) int32 {
	f := a.fail[u]
	for f != 0 && a.child(f, b) < 0 {
		f = a.fail[f]
	}
	if to := a.child(f, b); to >= 0 {
		return to
	}
	return 0
}

// step is the node after reading b at node at.
func (a *automaton) step(at int32, b byte) int32 {
	for {
		if to := a.child(at, b); to >= 0 {
			return to
		}
		if at == 0 {
			return 0
		}
		at = a.fail[at]
	}
}

// spans is mask.spans for the automaton's secrets.
func (a *automaton) spans(buf []byte, covered int) [][2]int {
	var found [][2]int
	if covered > 0 {
		found = append(found, [2]int{0, covered})
	}
	at := int32(0)
	for i, b := range buf {
		at = a.step(at, b)
		// The longest secret that ends here holds the shorter ones.
		n := at
		if !a.end[n] {
			n = a.dict[n]
		}
		if n >= 0 {
			found = append(found, [2]int{i + 1 - int(a.depth[n]), i + 1})
		}
	}
	slices.SortFunc(found, func(x, y [2]int) int { return x[0] - y[0] })
	var merged [][2]int
	for _, f := range found {
		if n := len(merged); n > 0 && f[0] <= merged[n-1][1] {
			merged[n-1][1] = max(merged[n-1][1], f[1])
		} else {
			merged = append(merged, f)
		}
	}
	return merged
}

// heldFrom is mask.heldFrom for the automaton's secrets: the node buf ends
// at is its longest suffix that starts a secret, and a node with no edge
// ends one and cannot grow, so the longest one that can is further down
// the fail chain.
func (a *automaton) heldFrom(buf []byte) int {
	at := int32(0)
	for _, b := range buf {
		at = a.step(at, b)
	}
	for at != 0 && a.first[at] < 0 {
		at = a.fail[at]
	}
	return len(buf) - int(a.depth[at])
}
