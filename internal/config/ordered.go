package config

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

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

const maxNodes = 10_000

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

type inliner struct {
	left   int
	within []*yaml.Node
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
