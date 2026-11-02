package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ordered decodes a YAML mapping into a slice of its values with their
// keys, keeping the file's order: params and stages are written as
// mappings and shown in the order they were written.
type ordered[T any] struct {
	items []namedItem[T]
}

type namedItem[T any] struct {
	name string
	item T
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (o *ordered[T]) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: want a mapping", n.Line)
	}
	in := &inliner{left: maxNodes}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key := n.Content[i]
		if key.Kind != yaml.ScalarNode {
			return fmt.Errorf("line %d: a key must be a plain name, not an alias or a collection", key.Line)
		}
		var item T
		if err := in.decodeStrict(n.Content[i+1], &item); err != nil {
			return fmt.Errorf("%s: %w", key.Value, err)
		}
		o.items = append(o.items, namedItem[T]{name: key.Value, item: item})
	}
	return nil
}

// maxNodes is the most YAML nodes all the params of a file, or all its
// stages, may come to once their aliases are written out. A real file has
// a few hundred; a few lines of nested aliases can stand for millions.
const maxNodes = 10_000

// decodeStrict decodes a node into the struct out points at, refusing
// unknown keys, which Node.Decode does not. The keys are checked here and
// not by writing the node out for a strict decoder to read again: that one
// would count its lines from the top of what was written out, and an error
// far down a file must name the line the file has it on.
func (in *inliner) decodeStrict(n *yaml.Node, out any) error {
	whole, err := in.copy(n)
	if err != nil {
		return err
	}
	if err := knownKeys(whole, reflect.TypeOf(out).Elem()); err != nil {
		return err
	}
	return whole.Decode(out)
}

// knownKeys refuses a key of the mapping that is not a field of the struct
// t, in the decoder's own words. A merge key brings in the keys of the
// mapping, or the mappings, it merges, and they are held to the same.
// Anything but a mapping is left for the decoder to refuse.
func knownKeys(n *yaml.Node, t reflect.Type) error {
	if n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.ShortTag() != "!!merge" {
			if err := knownField(key, value, t); err != nil {
				return err
			}
			continue
		}
		merged := []*yaml.Node{value}
		if value.Kind == yaml.SequenceNode {
			merged = value.Content
		}
		for _, m := range merged {
			if err := knownKeys(m, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// knownField refuses a key the struct has no field for; a nested struct (a
// stage's when), or each struct of a list of them (a stage's guards), is
// held to its own keys too.
func knownField(key, value *yaml.Node, t reflect.Type) error {
	ft, ok := fieldType(t, key.Value)
	if !ok {
		return fmt.Errorf("line %d: field %s not found in type %s", key.Line, key.Value, t)
	}
	items := []*yaml.Node{value}
	if ft.Kind() == reflect.Slice && value.Kind == yaml.SequenceNode {
		ft, items = ft.Elem(), value.Content
	}
	if ft.Kind() == reflect.Pointer {
		ft = ft.Elem()
	}
	if ft.Kind() != reflect.Struct {
		return nil
	}
	for _, item := range items {
		if err := knownKeys(item, ft); err != nil {
			return fmt.Errorf("%s: %w", key.Value, err)
		}
	}
	return nil
}

// fieldType is the type, through a pointer, of the struct's field read
// from the YAML key, and whether there is one.
func fieldType(t reflect.Type, key string) (reflect.Type, bool) {
	for field := range t.Fields() {
		if name, _, _ := strings.Cut(field.Tag.Get("yaml"), ","); name == key && name != "-" {
			ft := field.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			return ft, true
		}
	}
	return nil, false
}

// inliner copies a node with each alias replaced by what it points at, so
// that the copy stands without the rest of the file: an anchor may be
// defined anywhere in it.
type inliner struct {
	left   int          // nodes it may still copy, over every item of one mapping
	within []*yaml.Node // the nodes being copied; an alias to one has no end
}

func (in *inliner) copy(n *yaml.Node) (*yaml.Node, error) {
	if n.Kind == yaml.AliasNode {
		if slices.Contains(in.within, n.Alias) {
			return nil, fmt.Errorf("line %d: anchor %q holds itself", n.Line, n.Value)
		}
		return in.copy(n.Alias)
	}
	if in.left--; in.left < 0 {
		return nil, fmt.Errorf("line %d: more than %d values once its aliases are written out", n.Line, maxNodes)
	}
	c := *n
	c.Anchor = ""
	c.Content = make([]*yaml.Node, len(n.Content))
	in.within = append(in.within, n)
	for i, child := range n.Content {
		var err error
		if c.Content[i], err = in.copy(child); err != nil {
			return nil, err
		}
	}
	in.within = in.within[:len(in.within)-1]
	return &c, nil
}
