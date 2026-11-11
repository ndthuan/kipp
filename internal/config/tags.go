package config

import (
	"fmt"
	"path"
	"slices"
	"strconv"

	"go.yaml.in/yaml/v3"
)

// TagFilter says which pushed tags start builds: a tag matches when some
// Include pattern matches it (every tag when there is no Include) and no
// Exclude pattern does, so that an exclude wins. Off builds no tag. The
// patterns are path.Match's.
type TagFilter struct {
	Off              bool
	Include, Exclude []string
}

// Matches reports whether a push of tag starts a build.
func (f TagFilter) Matches(tag string) bool {
	if f.Off || matchesAny(f.Exclude, tag) {
		return false
	}
	return len(f.Include) == 0 || matchesAny(f.Include, tag)
}

const tagsShape = "a list of patterns like [\"v*\"], {include: [...], exclude: [...]}, true for every tag or false for none"

// UnmarshalYAML implements yaml.Unmarshaler. tags is a list (its patterns
// are the include, and an empty one builds no tag), a mapping of include
// and exclude, or a boolean: true builds every tag, false none.
func (f *TagFilter) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		on, err := strconv.ParseBool(n.Value)
		if err != nil || n.ShortTag() != "!!bool" {
			return fmt.Errorf("line %d: tags is %s", n.Line, tagsShape)
		}
		*f = TagFilter{Off: !on}
		return nil
	case yaml.SequenceNode:
		list, err := patterns(n)
		if err != nil {
			return err
		}
		*f = TagFilter{Include: list, Off: len(list) == 0}
		return nil
	case yaml.MappingNode:
		return f.fromMapping(n)
	}
	return fmt.Errorf("line %d: tags is %s", n.Line, tagsShape)
}

func (f *TagFilter) fromMapping(n *yaml.Node) error {
	var out TagFilter
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		list, err := patterns(value)
		if err != nil {
			return err
		}
		switch key.Value {
		case "include":
			if len(list) == 0 {
				return fmt.Errorf("line %d: tags.include is empty: leave it out for every tag, or say tags: false for none", key.Line)
			}
			out.Include = list
		case "exclude":
			out.Exclude = list
		default:
			return fmt.Errorf("line %d: tags takes include and exclude, not %s", key.Line, key.Value)
		}
	}
	*f = out
	return nil
}

// patterns reads a list of path.Match patterns.
func patterns(n *yaml.Node) ([]string, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("line %d: a list of tag patterns, like [\"v*\"]", n.Line)
	}
	out := []string{}
	for _, item := range n.Content {
		if item.Kind != yaml.ScalarNode {
			return nil, fmt.Errorf("line %d: a tag pattern is a string, like \"v*\"", item.Line)
		}
		out = append(out, item.Value)
	}
	return out, nil
}

// check refuses a pattern path.Match cannot read; where names the key.
func (f TagFilter) check(where string) error {
	for _, p := range append(slices.Clone(f.Include), f.Exclude...) {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return fmt.Errorf("%s: %q is not a name or a pattern path.Match reads", where, p)
		}
	}
	return nil
}
