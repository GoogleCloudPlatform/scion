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
// it parses to exactly the same data as the re-encoded tree, so the result is
// data-equivalent to the tree edit; comment placement is best-effort.

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// errYAMLEditThroughAlias is returned when an edit would have to write
// through a YAML alias or into an anchored node. Writing into an anchored
// node would silently change every alias that references it, so callers fall
// back to a full decode/encode that expands the aliases instead.
var errYAMLEditThroughAlias = errors.New("yaml edit path goes through an alias or anchor")

// isYAMLShared reports whether editing n in place could affect other parts
// of the document: n is an alias, or carries an anchor that aliases can
// refer to.
func isYAMLShared(n *yaml.Node) bool {
	return n != nil && (n.Kind == yaml.AliasNode || n.Anchor != "")
}

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
//
// It returns false when the existing value already equals value; root is
// then unmodified, because a null intermediate can only be met when the key
// below it is missing, and that edit always changes something. It returns
// errYAMLEditThroughAlias, before modifying anything, when a node on the
// path (the target value included) is an alias or carries an anchor.
func setYAMLPath(root *yaml.Node, path []string, value *yaml.Node) (bool, error) {
	if len(path) == 0 {
		return false, errors.New("empty yaml path")
	}
	if err := checkYAMLPathUnshared(root, path); err != nil {
		return false, err
	}
	m := root
	for i, k := range path {
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

// checkYAMLPathUnshared returns errYAMLEditThroughAlias if root or any
// existing node along path (the target value included) is an alias or
// carries an anchor.
func checkYAMLPathUnshared(root *yaml.Node, path []string) error {
	m := root
	if isYAMLShared(m) {
		return errYAMLEditThroughAlias
	}
	for _, k := range path {
		if m.Kind != yaml.MappingNode {
			return nil
		}
		_, v := findMapKey(m, k)
		if v == nil {
			return nil
		}
		if isYAMLShared(v) {
			return errYAMLEditThroughAlias
		}
		m = v
	}
	return nil
}

// deleteYAMLPath removes the key at path from the mapping node root. It
// returns false when the key (or one of its parents) does not exist.
// Parents left empty are kept, as an empty mapping. Like setYAMLPath it
// refuses to edit through an alias or anchored node.
func deleteYAMLPath(root *yaml.Node, path []string) (bool, error) {
	if len(path) == 0 {
		return false, errors.New("empty yaml path")
	}
	if err := checkYAMLPathUnshared(root, path); err != nil {
		return false, err
	}
	m := root
	var parentKey *yaml.Node
	for i, k := range path {
		if m.Kind != yaml.MappingNode {
			// A null or scalar parent has no children to delete.
			return false, nil
		}
		kn, v := findMapKey(m, k)
		if v == nil {
			return false, nil
		}
		if i == len(path)-1 {
			deleteMapKey(m, k)
			if len(m.Content) == 0 && parentKey != nil {
				// Encode the emptied mapping as `parent: {}`. yaml.v3 emits
				// an unparseable `parent: # comment\n{}` for an empty block
				// mapping whose key has a line comment, so the comment moves
				// to the flow value: `parent: {} # comment`.
				m.Style |= yaml.FlowStyle
				if m.LineComment == "" {
					m.LineComment, parentKey.LineComment = parentKey.LineComment, ""
				}
			}
			return true, nil
		}
		parentKey = kn
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

// yamlDecodesTo reports whether data parses and decodes to want (generic
// data from decoding a yaml.Node), ignoring comments and layout.
func yamlDecodesTo(data []byte, want interface{}) bool {
	var got interface{}
	if err := yaml.Unmarshal(data, &got); err != nil {
		return false
	}
	return reflect.DeepEqual(got, want)
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

// yamlLineEnding returns the line terminator of line ("\r\n" or "\n"), or
// "" if it has none.
func yamlLineEnding(line []byte) string {
	switch {
	case bytes.HasSuffix(line, []byte("\r\n")):
		return "\r\n"
	case bytes.HasSuffix(line, []byte("\n")):
		return "\n"
	}
	return ""
}

// insertLineEnding picks the terminator for lines inserted after
// lines[idx-1]: that line's own, else the first terminated line's, else LF.
func insertLineEnding(lines [][]byte, idx int) string {
	if idx > 0 {
		if eol := yamlLineEnding(lines[idx-1]); eol != "" {
			return eol
		}
	}
	for _, l := range lines {
		if eol := yamlLineEnding(l); eol != "" {
			return eol
		}
	}
	return "\n"
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
	eol := insertLineEnding(lines, idx)
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
		block = append(block, eol...)
	}
	out := make([][]byte, 0, len(lines)+1)
	out = append(out, lines[:idx]...)
	if prev := out[idx-1]; yamlLineEnding(prev) == "" {
		out[idx-1] = append(append([]byte(nil), prev...), eol...)
	}
	out = append(out, block)
	out = append(out, lines[idx:]...)
	return joinYAMLLines(out), true
}

// spliceDeleteYAMLPath applies deleteYAMLPath(root, path) to orig at the
// byte level by dropping the line of a `key: scalar` entry. When that entry
// is the only child of a nested block mapping, the parent's line becomes
// `parent: {}`, matching the empty mapping the tree edit leaves. root must
// be the unedited parse of orig. Comments above the entry are left in place.
func spliceDeleteYAMLPath(orig []byte, root *yaml.Node, path []string) ([]byte, bool) {
	if len(path) == 0 {
		return nil, false
	}
	lines := splitYAMLLines(orig)
	m := root
	var parentKey *yaml.Node
	for i, k := range path {
		if !isBlockYAMLMapping(m) {
			return nil, false
		}
		kn, v := findMapKey(m, k)
		if v == nil {
			return nil, false
		}
		if i < len(path)-1 {
			parentKey = kn
			m = v
			continue
		}
		if v.Kind != yaml.ScalarNode || v.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 ||
			kn.Line != v.Line || kn.Line < 1 || kn.Line > len(lines) {
			return nil, false
		}
		out := append([][]byte(nil), lines...)
		if len(m.Content) == 2 && parentKey != nil {
			// Emptying a nested mapping: turn `parent:` into `parent: {}`.
			if parentKey.Line >= kn.Line || parentKey.Line < 1 {
				return nil, false
			}
			rewritten, ok := appendEmptyMappingToKeyLine(out[parentKey.Line-1], parentKey)
			if !ok {
				return nil, false
			}
			out[parentKey.Line-1] = rewritten
		}
		out = append(out[:kn.Line-1], out[kn.Line:]...)
		return joinYAMLLines(out), true
	}
	return nil, false
}

// appendEmptyMappingToKeyLine rewrites the line holding the plain mapping
// key kn (`  key:` with an optional trailing comment) as `  key: {}`.
func appendEmptyMappingToKeyLine(line []byte, kn *yaml.Node) ([]byte, bool) {
	if kn.Kind != yaml.ScalarNode || kn.Style != 0 {
		return nil, false
	}
	start, ok := runeColumnToByteOffset(line, kn.Column, kn.Line == 1)
	if !ok || !bytes.HasPrefix(line[start:], []byte(kn.Value)) {
		return nil, false
	}
	colon := start + len(kn.Value)
	for colon < len(line) && line[colon] == ' ' {
		colon++
	}
	if colon >= len(line) || line[colon] != ':' {
		return nil, false
	}
	out := make([]byte, 0, len(line)+3)
	out = append(out, line[:colon+1]...)
	out = append(out, " {}"...)
	out = append(out, line[colon+1:]...)
	return out, true
}

// utf8BOM is the byte-order-mark yaml.v3 skips before counting columns, but
// which is still physically present at the start of the original file.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// runeColumnToByteOffset converts a 1-indexed, rune-counted yaml.v3 Column
// on line into a 0-indexed byte offset. firstLine skips a leading UTF-8 BOM
// before counting, matching yaml.v3's own column numbering, while still
// returning an offset relative to line's real bytes (BOM included).
func runeColumnToByteOffset(line []byte, column int, firstLine bool) (int, bool) {
	if column < 1 {
		return 0, false
	}
	rest := line
	prefix := 0
	if firstLine && bytes.HasPrefix(rest, utf8BOM) {
		prefix = len(utf8BOM)
		rest = rest[prefix:]
	}
	runeIdx := 1
	byteIdx := 0
	for byteIdx < len(rest) {
		if runeIdx == column {
			return prefix + byteIdx, true
		}
		_, size := utf8.DecodeRune(rest[byteIdx:])
		if size == 0 {
			return 0, false
		}
		byteIdx += size
		runeIdx++
	}
	if runeIdx == column {
		return prefix + byteIdx, true
	}
	return 0, false
}
