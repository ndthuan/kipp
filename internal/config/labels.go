package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Labels name a place a stage may run: a stage asks for labels, a runner
// or an engine carries them, and a stage runs where every label it asks
// for is carried. A job file writes one label or a list of them.
type Labels []string

// ValidLabel reports whether s may be a label: the same characters as a
// name, so a label is safe in a log line, a URL and a container label.
func ValidLabel(s string) bool { return nameRe.MatchString(s) }

// UnmarshalYAML implements yaml.Unmarshaler.
func (l *Labels) UnmarshalYAML(n *yaml.Node) error {
	items := []*yaml.Node{n}
	if n.Kind == yaml.SequenceNode {
		items = n.Content
	}
	if len(items) == 0 {
		return fmt.Errorf("line %d: runs_on is a label or a list of one or more labels, like linux or [linux, arm64]", n.Line)
	}
	var out Labels
	for _, item := range items {
		if item.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: runs_on is a label or a list of one or more labels, like linux or [linux, arm64]", item.Line)
		}
		if !ValidLabel(item.Value) {
			return fmt.Errorf("line %d: label %q: lower-case letters, digits, dot, dash and underscore, starting with a letter or digit", item.Line, item.Value)
		}
		if out.Has(item.Value) {
			return fmt.Errorf("line %d: runs_on names %s twice", item.Line, item.Value)
		}
		out = append(out, item.Value)
	}
	*l = out
	return nil
}

// Has reports whether the labels hold s.
func (l Labels) Has(s string) bool { return slices.Contains(l, s) }

// String is the labels comma-joined, as a command line takes them.
func (l Labels) String() string { return strings.Join(l, ",") }

// ParseLabels reads labels written comma-separated, as a command line
// takes them: one or more, each a label, none twice.
func ParseLabels(list string) (Labels, error) {
	var out Labels
	for item := range strings.SplitSeq(list, ",") {
		item = strings.TrimSpace(item)
		switch {
		case item == "":
			continue
		case !ValidLabel(item):
			return nil, fmt.Errorf("label %q: lower-case letters, digits, dot, dash and underscore, starting with a letter or digit", item)
		case out.Has(item):
			return nil, fmt.Errorf("label %s twice", item)
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil, errors.New("one or more labels, comma-separated, like linux,arm64")
	}
	return out, nil
}
