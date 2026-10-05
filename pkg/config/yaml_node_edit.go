// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package config

// Comment-preserving edits of YAML settings files.
//
// Settings files are hand-maintained, so a write-back of one key must not
// reflow the whole file. These helpers edit a parsed yaml.Node tree in place
// (setYAMLPath / deleteYAMLPath) and, where possible, apply the same edit as
// a byte-level splice of the original file (spliceSetYAMLPath /
// spliceDeleteYAMLPath). A splice keeps blank lines, sequence indentation and
// quoting that a yaml.v3 re-encode would normalise away. It is used only when
// it parses to exactly the same data as the re-encoded tree, so it cannot
// produce a different result from the tree edit.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// errYAMLEditThroughAlias is returned when an edit would have to write
// through a YAML alias. Writing into the anchored node would silently change
// every other place that references it, so callers fall back to a full
// decode/encode that expands the alias instead.
var errYAMLEditThroughAlias = errors.New("yaml edit path goes through an alias")

// resolveAlias follows n through any YAML anchors/aliases (`key: *v`) to
// the node it actually refers to, so value comparisons and the in-memory
// override read the real value rather than the anchor name. Returns nil for
// a dangling alias. Non-alias nodes are returned unchanged.
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// findChildMapping returns root itself for name == "", or the mapping node
// of the top-level key name within root (nil if absent or not a mapping,
// following an alias first so `hub: *anchor` resolves to the real mapping).
func findChildMapping(root *yaml.Node, name string) *yaml.Node {
	if name == "" {
		return root
	}
	_, val := findMapKey(root, name)
	val = resolveAlias(val)
	if val == nil || val.Kind != yaml.MappingNode {
		return nil
	}
	return val
}

// findMapKey returns the key and value nodes for name in mapping's Content
// (alternating key/value pairs), or nil, nil if mapping is nil or has no
// such key.
func findMapKey(mapping *yaml.Node, name string) (key, value *yaml.Node) {
	if mapping == nil {
		return nil, nil
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// deleteMapKey removes name's key/value pair from mapping's Content, if
// present.
func deleteMapKey(mapping *yaml.Node, name string) {
	if mapping == nil {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == name {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// newYAMLStringScalar returns a !!str scalar node for s. The encoder quotes
// it when the plain form would resolve to another type ("true", "123").
func newYAMLStringScalar(s string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// newYAMLBoolScalar returns a !!bool scalar node for b.
func newYAMLBoolScalar(b bool) *yaml.Node {
	v := "false"
	if b {
		v = "true"
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
}

// isYAMLNull reports whether n is a null scalar (`key:` or `key: ~`).
func isYAMLNull(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

// yamlPathString renders path for error messages.
func yamlPathString(path []string) string {
	if len(path) == 0 {
		return "<document>"
	}
	return strings.Join(path, ".")
}

// setYAMLPath sets path within the mapping node root to value, creating any
// missing intermediate mappings (a null intermediate such as `hub:` becomes a
// mapping). New keys are appended after the existing keys of their mapping,
// so key order is preserved. An existing scalar value is updated in place,
// keeping its comments and, for string-to-string updates, its quoting style.
// It returns false, and leaves root untouched, when the existing value
// already equals value.
func setYAMLPath(root *yaml.Node, path []string, value *yaml.Node) (bool, error) {
	if len(path) == 0 {
		return false, errors.New("empty yaml path")
	}
	m := root
	for i, k := range path {
		if m.Kind == yaml.AliasNode {
			return false, errYAMLEditThroughAlias
		}
		if isYAMLNull(m) {
			m.Kind, m.Tag, m.Value, m.Style = yaml.MappingNode, "!!map", "", 0
		}
		if m.Kind != yaml.MappingNode {
			return false, fmt.Errorf("cannot set %s: %s is not a mapping", yamlPathString(path), yamlPathString(path[:i]))
		}
		_, v := findMapKey(m, k)
		last := i == len(path)-1
		if v == nil {
			if last {
				m.Content = append(m.Content, newYAMLStringScalar(k), value)
				return true, nil
			}
			child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			m.Content = append(m.Content, newYAMLStringScalar(k), child)
			m = child
			continue
		}
		if last {
			return replaceYAMLMapValue(m, k, v, value), nil
		}
		m = v
	}
	return false, nil // unreachable: the loop returns on the last element
}

// replaceYAMLMapValue replaces old (the value of key k in mapping m) with
// value, unless they are already equal. A scalar old node is updated in
// place so its comments stay attached.
func replaceYAMLMapValue(m *yaml.Node, k string, old, value *yaml.Node) bool {
	if yamlScalarsEqual(old, value) {
		return false
	}
	if old.Kind == yaml.ScalarNode && value.Kind == yaml.ScalarNode {
		r := replacementScalar(old, value)
		old.Tag, old.Value, old.Style = r.Tag, r.Value, r.Style
		return true
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == k {
			value.HeadComment = old.HeadComment
			value.LineComment = old.LineComment
			value.FootComment = old.FootComment
			m.Content[i+1] = value
			return true
		}
	}
	return false
}

// yamlScalarsEqual reports whether a and b are scalars with the same
// resolved tag and value.
func yamlScalarsEqual(a, b *yaml.Node) bool {
	return a != nil && b != nil &&
		a.Kind == yaml.ScalarNode && b.Kind == yaml.ScalarNode &&
		a.ShortTag() == b.ShortTag() && a.Value == b.Value
}

// replacementScalar returns the scalar that should replace old: value's tag
// and text, keeping old's single or double quoting when both are strings.
func replacementScalar(old, value *yaml.Node) *yaml.Node {
	r := &yaml.Node{Kind: yaml.ScalarNode, Tag: value.Tag, Value: value.Value}
	quoted := old.Style & (yaml.SingleQuotedStyle | yaml.DoubleQuotedStyle)
	if quoted != 0 && old.ShortTag() == "!!str" && value.ShortTag() == "!!str" && !strings.Contains(value.Value, "\n") {
		r.Style = quoted
	}
	return r
}

// deleteYAMLPath removes the key at path from the mapping node root. It
// returns false when the key (or one of its parents) does not exist.
// Parents left empty are kept, as an empty mapping.
func deleteYAMLPath(root *yaml.Node, path []string) (bool, error) {
	if len(path) == 0 {
		return false, errors.New("empty yaml path")
	}
	m := root
	for i, k := range path {
		if m.Kind == yaml.AliasNode {
			return false, errYAMLEditThroughAlias
		}
		if m.Kind != yaml.MappingNode {
			// A null or scalar parent has no children to delete.
			return false, nil
		}
		_, v := findMapKey(m, k)
		if v == nil {
			return false, nil
		}
		if i == len(path)-1 {
			deleteMapKey(m, k)
			return true, nil
		}
		m = v
	}
	return false, nil // unreachable: the loop returns on the last element
}

// parseYAMLMappingDocument parses data into a document node whose content
// is a single mapping. Empty, comment-only and null documents become an
// empty mapping; any other non-mapping document is an error.
func parseYAMLMappingDocument(data []byte) (*yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode}
	}
	if doc.Kind != yaml.DocumentNode {
		return nil, fmt.Errorf("unexpected YAML node kind %v at top level", doc.Kind)
	}
	if len(doc.Content) == 0 {
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}
	root := doc.Content[0]
	if isYAMLNull(root) {
		root.Kind, root.Tag, root.Value, root.Style = yaml.MappingNode, "!!map", "", 0
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top-level YAML value is a %s, not a mapping", root.ShortTag())
	}
	return &doc, nil
}

// detectYAMLIndent returns the indentation step used by the block mappings
// in root, or 2 when root has no nested block mapping to measure.
func detectYAMLIndent(root *yaml.Node) int {
	queue := []*yaml.Node{root}
	for len(queue) > 0 {
		m := queue[0]
		queue = queue[1:]
		if m.Kind != yaml.MappingNode || m.Style&yaml.FlowStyle != 0 {
			continue
		}
		for i := 0; i+1 < len(m.Content); i += 2 {
			k, v := m.Content[i], m.Content[i+1]
			if v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle == 0 && len(v.Content) > 0 {
				if step := v.Content[0].Column - k.Column; step >= 2 && step <= 8 && v.Content[0].Line > k.Line {
					return step
				}
				queue = append(queue, v)
			}
		}
	}
	return 2
}

// encodeYAMLDocument encodes doc with the given indentation step.
func encodeYAMLDocument(doc *yaml.Node, indent int) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlSemanticallyEqual reports whether a and b both parse and decode to
// the same data, ignoring comments and layout.
func yamlSemanticallyEqual(a, b []byte) bool {
	var x, y interface{}
	if err := yaml.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := yaml.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// encodeYAMLInline encodes the scalar n as a single line of YAML (quoted
// if needed), or reports false if it does not fit on one line.
func encodeYAMLInline(n *yaml.Node) (string, bool) {
	out, err := yaml.Marshal(n)
	if err != nil {
		return "", false
	}
	s := strings.TrimSuffix(string(out), "\n")
	if s == "" || strings.Contains(s, "\n") {
		return "", false
	}
	return s, true
}

// splitYAMLLines splits data into lines that keep their "\n" terminators.
func splitYAMLLines(data []byte) [][]byte {
	lines := bytes.SplitAfter(data, []byte("\n"))
	if n := len(lines); n > 0 && len(lines[n-1]) == 0 {
		lines = lines[:n-1]
	}
	return lines
}

// joinYAMLLines is the inverse of splitYAMLLines.
func joinYAMLLines(lines [][]byte) []byte {
	return bytes.Join(lines, nil)
}

// maxYAMLLine returns the largest source line of n or any node below it
// (not following aliases).
func maxYAMLLine(n *yaml.Node) int {
	maxLine := n.Line
	for _, c := range n.Content {
		if l := maxYAMLLine(c); l > maxLine {
			maxLine = l
		}
	}
	return maxLine
}

// isBlockYAMLMapping reports whether n is a non-empty block-style mapping,
// the only kind of mapping a splice can insert into.
func isBlockYAMLMapping(n *yaml.Node) bool {
	return n != nil && n.Kind == yaml.MappingNode && n.Style&yaml.FlowStyle == 0 && len(n.Content) >= 2
}

// spliceSetYAMLPath applies setYAMLPath(root, path, value) to orig at the
// byte level: an existing single-line scalar is replaced in place, and a
// missing key (with any missing parents) is inserted as new lines after the
// last entry of its closest existing block mapping. root must be the
// unedited parse of orig. It reports false when the edit cannot be spliced;
// callers must still check the result against the tree edit.
func spliceSetYAMLPath(orig []byte, root *yaml.Node, path []string, value *yaml.Node, indent int) ([]byte, bool) {
	if !isBlockYAMLMapping(root) || len(path) == 0 {
		return nil, false
	}
	lines := splitYAMLLines(orig)
	m := root
	for i, k := range path {
		_, v := findMapKey(m, k)
		if v == nil {
			return spliceInsertYAMLKeys(lines, m, path[i:], value, indent)
		}
		if i == len(path)-1 {
			return spliceReplaceYAMLScalar(lines, v, value)
		}
		if !isBlockYAMLMapping(v) {
			return nil, false
		}
		m = v
	}
	return nil, false
}

// spliceReplaceYAMLScalar replaces the single-line scalar old with value.
func spliceReplaceYAMLScalar(lines [][]byte, old, value *yaml.Node) ([]byte, bool) {
	if old.Kind != yaml.ScalarNode || value.Kind != yaml.ScalarNode ||
		old.Style&(yaml.LiteralStyle|yaml.FoldedStyle|yaml.TaggedStyle) != 0 ||
		old.Line < 1 || old.Line > len(lines) {
		return nil, false
	}
	line := lines[old.Line-1]
	start, ok := runeColumnToByteOffset(line, old.Column, old.Line == 1)
	if !ok {
		return nil, false
	}
	end := -1
	switch {
	case old.Style&yaml.DoubleQuotedStyle != 0:
		for j := start + 1; j < len(line); j++ {
			if line[j] == '\\' {
				j++
				continue
			}
			if line[j] == '"' {
				end = j + 1
				break
			}
		}
	case old.Style&yaml.SingleQuotedStyle != 0:
		for j := start + 1; j < len(line); j++ {
			if line[j] != '\'' {
				continue
			}
			if j+1 < len(line) && line[j+1] == '\'' {
				j++
				continue
			}
			end = j + 1
			break
		}
	default:
		if old.Value == "" || !bytes.HasPrefix(line[start:], []byte(old.Value)) {
			return nil, false
		}
		end = start + len(old.Value)
	}
	if end < 0 {
		return nil, false
	}
	text, ok := encodeYAMLInline(replacementScalar(old, value))
	if !ok {
		return nil, false
	}
	newLine := make([]byte, 0, len(line)+len(text))
	newLine = append(newLine, line[:start]...)
	newLine = append(newLine, text...)
	newLine = append(newLine, line[end:]...)
	out := append([][]byte(nil), lines...)
	out[old.Line-1] = newLine
	return joinYAMLLines(out), true
}

// spliceInsertYAMLKeys inserts keys (nested, the last one set to value) as
// new lines after the last entry of the block mapping m.
func spliceInsertYAMLKeys(lines [][]byte, m *yaml.Node, keys []string, value *yaml.Node, indent int) ([]byte, bool) {
	if !isBlockYAMLMapping(m) || value.Kind != yaml.ScalarNode {
		return nil, false
	}
	base := m.Content[0].Column - 1
	if base < 0 {
		return nil, false
	}
	// The entry ends at its last node's line, plus any following lines that
	// are indented deeper than m's keys: block scalar bodies, continuation
	// lines and trailing comments that belong to the last entry.
	idx := maxYAMLLine(m)
	if idx < 1 || idx > len(lines) {
		return nil, false
	}
	for idx < len(lines) {
		l := lines[idx]
		trimmed := bytes.TrimLeft(l, " ")
		if len(bytes.TrimSpace(l)) == 0 || len(l)-len(trimmed) <= base {
			break
		}
		idx++
	}
	valueText, ok := encodeYAMLInline(value)
	if !ok {
		return nil, false
	}
	var block []byte
	for j, k := range keys {
		keyText, ok := encodeYAMLInline(newYAMLStringScalar(k))
		if !ok {
			return nil, false
		}
		block = append(block, strings.Repeat(" ", base+j*indent)...)
		block = append(block, keyText...)
		block = append(block, ':')
		if j == len(keys)-1 {
			block = append(block, ' ')
			block = append(block, valueText...)
		}
		block = append(block, '\n')
	}
	out := make([][]byte, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	if prev := out[idx-1]; !bytes.HasSuffix(prev, []byte("\n")) {
		out[idx-1] = append(append([]byte(nil), prev...), '\n')
	}
	out = append(out, block)
	out = append(out, lines[idx:]...)
	return joinYAMLLines(out), true
}

// spliceDeleteYAMLPath applies deleteYAMLPath(root, path) to orig at the
// byte level by dropping the line of a `key: scalar` entry. root must be
// the unedited parse of orig. Comments above the entry are left in place.
func spliceDeleteYAMLPath(orig []byte, root *yaml.Node, path []string) ([]byte, bool) {
	if len(path) == 0 {
		return nil, false
	}
	lines := splitYAMLLines(orig)
	m := root
	for i, k := range path {
		if !isBlockYAMLMapping(m) {
			return nil, false
		}
		kn, v := findMapKey(m, k)
		if v == nil {
			return nil, false
		}
		if i < len(path)-1 {
			m = v
			continue
		}
		if v.Kind != yaml.ScalarNode || v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 ||
			kn.Line != v.Line || kn.Line < 1 || kn.Line > len(lines) {
			return nil, false
		}
		out := make([][]byte, 0, len(lines)-1)
		out = append(out, lines[:kn.Line-1]...)
		out = append(out, lines[kn.Line:]...)
		return joinYAMLLines(out), true
	}
	return nil, false
}
