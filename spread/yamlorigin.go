package spread

import (
	"path/filepath"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
)

// YAMLOrigin is the YAML file and line of bash line 1 of a stage body.
type YAMLOrigin struct {
	File string
	Line int
}

func yamlRelFile(projectPath, filename string) string {
	rel, err := filepath.Rel(projectPath, filename)
	if err != nil {
		return filepath.Base(filename)
	}
	return filepath.ToSlash(rel)
}

func locateYAMLOrigin(relFile string, data []byte, body string, keys ...string) YAMLOrigin {
	first := firstTrimmedLine(body)
	if relFile == "" || first == "" {
		return YAMLOrigin{}
	}
	node := yamlLookup(data, keys...)
	if node == nil {
		return YAMLOrigin{}
	}
	line := findLineFrom(data, node.Line, first)
	if line <= 0 {
		return YAMLOrigin{}
	}
	return YAMLOrigin{File: relFile, Line: line}
}

func yamlBodyOrigin(data []byte, keys ...string) int {
	node := yamlLookup(data, keys...)
	if node == nil {
		return 0
	}
	first := firstTrimmedLine(node.Value)
	if first == "" {
		return 0
	}
	return findLineFrom(data, node.Line, first)
}

func yamlLookup(data []byte, keys ...string) *yamlv3.Node {
	var root yamlv3.Node
	if err := yamlv3.Unmarshal(data, &root); err != nil {
		return nil
	}
	n := &root
	for _, key := range keys {
		n = yamlChild(n, key)
		if n == nil {
			return nil
		}
	}
	return n
}

func yamlChild(n *yamlv3.Node, key string) *yamlv3.Node {
	n = yamlUnwrap(n)
	if n == nil {
		return nil
	}
	switch n.Kind {
	case yamlv3.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := yamlUnwrap(n.Content[i])
			if k != nil && k.Kind == yamlv3.ScalarNode && k.Value == key {
				return n.Content[i+1]
			}
		}
	case yamlv3.SequenceNode:
		idx, err := strconv.Atoi(key)
		if err != nil || idx < 0 || idx >= len(n.Content) {
			return nil
		}
		return n.Content[idx]
	}
	return nil
}

func yamlUnwrap(n *yamlv3.Node) *yamlv3.Node {
	for n != nil {
		switch n.Kind {
		case yamlv3.DocumentNode:
			if len(n.Content) == 0 {
				return nil
			}
			n = n.Content[0]
		case yamlv3.AliasNode:
			n = n.Alias
		default:
			return n
		}
	}
	return nil
}

func firstTrimmedLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func findLineFrom(data []byte, startLine int, first string) int {
	if first == "" {
		return 0
	}
	lines := strings.Split(string(data), "\n")
	start := startLine - 1
	if start < 0 {
		start = 0
	}
	if start > len(lines) {
		start = 0
	}
	for i := start; i < len(lines); i++ {
		if strings.TrimSpace(strings.TrimSuffix(lines[i], "\r")) == first {
			return i + 1
		}
	}
	return 0
}
