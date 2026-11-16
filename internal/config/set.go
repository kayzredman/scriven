package config

import (
	"bytes"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Set changes one dotted key (e.g. "notify.channels", "merge_policy.dev.auto_merge",
// "scope.targets.0.context") in the file at path, keeping comments and key order.
// The value is parsed as YAML, so "[teams, email]" becomes a list and "2" a number.
// It returns the resulting config without saving; call SaveSet to write it.
func Set(path, key, value string) (*Config, []byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil, fmt.Errorf("%s is empty", path)
	}

	var val yaml.Node
	if err := yaml.Unmarshal([]byte(value), &val); err != nil {
		return nil, nil, fmt.Errorf("value: %w", err)
	}
	newNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
	if len(val.Content) > 0 {
		newNode = val.Content[0]
	}

	if err := setPath(doc.Content[0], strings.Split(key, "."), newNode); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key, err)
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, nil, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", key, err)
	}
	return &c, buf.Bytes(), nil
}

func setPath(n *yaml.Node, parts []string, v *yaml.Node) error {
	head, rest := parts[0], parts[1:]
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Value == head {
				if len(rest) == 0 {
					n.Content[i+1] = v
					return nil
				}
				return setPath(n.Content[i+1], rest, v)
			}
		}
		// Key not present: create it (and intermediate maps).
		k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: head}
		if len(rest) == 0 {
			n.Content = append(n.Content, k, v)
			return nil
		}
		child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		n.Content = append(n.Content, k, child)
		return setPath(child, rest, v)
	case yaml.SequenceNode:
		i, err := strconv.Atoi(head)
		if err != nil || i < 0 || i >= len(n.Content) {
			return fmt.Errorf("%q is not a valid index (list has %d items)", head, len(n.Content))
		}
		if len(rest) == 0 {
			n.Content[i] = v
			return nil
		}
		return setPath(n.Content[i], rest, v)
	default:
		return fmt.Errorf("cannot descend into %q: it is a single value", head)
	}
}
