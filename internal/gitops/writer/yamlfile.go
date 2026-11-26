/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package writer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

// yamlFile is a parsed (possibly multi-document) YAML file that records edits so
// that it can be written back with the smallest possible textual diff.
//
// When only existing scalar values change, the original bytes are patched in
// place, so comments, blank lines, quoting and indentation are untouched. When
// keys must be inserted, the node tree is re-encoded with a two-space indent.
const (
	tagMap = "!!map"
	tagStr = "!!str"
)

type yamlFile struct {
	src        []byte
	docs       []*yaml.Node
	edits      []scalarEdit
	structural bool
}

type scalarEdit struct {
	node     *yaml.Node
	oldValue string
	oldStyle yaml.Style
	newValue string
}

// parseYAMLFile parses every document in content.
func parseYAMLFile(content []byte) (*yamlFile, error) {
	f := &yamlFile{src: content}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("failed to parse YAML: %w", err)
		}
		f.docs = append(f.docs, &doc)
	}
	if len(f.docs) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}
	return f, nil
}

// rootMapping returns the top-level mapping of document i.
func (f *yamlFile) rootMapping(i int) (*yaml.Node, error) {
	doc := f.docs[i]
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("invalid YAML document")
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping node at document root")
	}
	return doc.Content[0], nil
}

// ensureMapping returns the mapping stored under key in parent, creating it if needed.
func (f *yamlFile) ensureMapping(parent *yaml.Node, key string) (*yaml.Node, error) {
	idx := findKeyIndex(parent, key)
	if idx == -1 {
		f.markStructural(parent)
		node := &yaml.Node{Kind: yaml.MappingNode, Tag: tagMap}
		parent.Content = append(parent.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: key}, node)
		return node, nil
	}
	node := parent.Content[idx+1]
	if node.Kind == yaml.ScalarNode && (node.Tag == "!!null" || node.Value == "" || node.Value == "~") {
		// "resources:" or "resources: ~" with no value: turn it into a mapping.
		f.markStructural(parent)
		*node = yaml.Node{Kind: yaml.MappingNode, Tag: tagMap, LineComment: node.LineComment}
		return node, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected mapping at key %q", key)
	}
	return node, nil
}

// markStructural records that mapping is about to receive new keys.
func (f *yamlFile) markStructural(_ *yaml.Node) {
	f.structural = true
}

// setScalar sets mapping[key] = value, recording the edit.
func (f *yamlFile) setScalar(mapping *yaml.Node, key, value string) {
	idx := findKeyIndex(mapping, key)
	if idx == -1 {
		f.markStructural(mapping)
		mapping.Content = append(mapping.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: tagStr, Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Value: value})
		return
	}
	node := mapping.Content[idx+1]
	if node.Kind != yaml.ScalarNode {
		f.structural = true
		mapping.Content[idx+1] = &yaml.Node{Kind: yaml.ScalarNode, Value: value}
		return
	}
	if sameQuantity(node.Value, value) {
		return
	}
	f.edits = append(f.edits, scalarEdit{node: node, oldValue: node.Value, oldStyle: node.Style, newValue: value})
	node.Value = value
	// Let the value resolve its own tag: "500m" must not keep an "!!int" tag from "1".
	node.Tag = ""
}

// sameQuantity reports whether two strings denote the same value, so "1" and
// "1000m" are not rewritten into each other on every run.
func sameQuantity(a, b string) bool {
	if a == b {
		return true
	}
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	return errA == nil && errB == nil && qa.Cmp(qb) == 0
}

// render returns the updated file content.
func (f *yamlFile) render() ([]byte, error) {
	if !f.structural {
		if len(f.edits) == 0 {
			return f.src, nil
		}
		if out, ok := f.patchInPlace(); ok {
			return out, nil
		}
	}
	return f.reencode()
}

// reencode serializes the edited node tree.
func (f *yamlFile) reencode() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range f.docs {
		if err := enc.Encode(doc); err != nil {
			return nil, fmt.Errorf("failed to encode YAML: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("failed to encode YAML: %w", err)
	}
	return buf.Bytes(), nil
}

// patchInPlace rewrites only the bytes of edited scalars. It returns ok=false if
// any edit cannot be located exactly or the result does not parse back to the
// edited tree, in which case the caller falls back to re-encoding.
func (f *yamlFile) patchInPlace() ([]byte, bool) {
	lineStarts := []int{0}
	for i, b := range f.src {
		if b == '\n' {
			lineStarts = append(lineStarts, i+1)
		}
	}

	type replacement struct {
		start, end int
		text       string
	}
	reps := make([]replacement, 0, len(f.edits))
	for _, e := range f.edits {
		if e.node.Line < 1 || e.node.Line > len(lineStarts) {
			return nil, false
		}
		start := lineStarts[e.node.Line-1]
		// yaml.v3 columns count characters, not bytes.
		for c := 1; c < e.node.Column; c++ {
			if start >= len(f.src) {
				return nil, false
			}
			_, size := utf8.DecodeRune(f.src[start:])
			start += size
		}

		var token, replacementText string
		switch e.oldStyle {
		case 0:
			token, replacementText = e.oldValue, e.newValue
			if needsQuoting(e.newValue) {
				replacementText = fmt.Sprintf("%q", e.newValue)
			}
		case yaml.DoubleQuotedStyle:
			token, replacementText = `"`+e.oldValue+`"`, `"`+e.newValue+`"`
		case yaml.SingleQuotedStyle:
			token, replacementText = `'`+e.oldValue+`'`, `'`+e.newValue+`'`
		default:
			return nil, false
		}
		end := start + len(token)
		if end > len(f.src) || string(f.src[start:end]) != token {
			return nil, false
		}
		if e.oldStyle == 0 && end < len(f.src) && !isPlainScalarTerminator(f.src[end]) {
			return nil, false
		}
		reps = append(reps, replacement{start: start, end: end, text: replacementText})
	}

	sort.Slice(reps, func(i, j int) bool { return reps[i].start > reps[j].start })
	out := append([]byte(nil), f.src...)
	for i, r := range reps {
		if i > 0 && r.end > reps[i-1].start {
			return nil, false
		}
		out = append(out[:r.start], append([]byte(r.text), out[r.end:]...)...)
	}

	if !f.equivalent(out) {
		return nil, false
	}
	return out, true
}

func isPlainScalarTerminator(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', ']', '}', '#':
		return true
	}
	return false
}

// needsQuoting reports whether a value cannot be written as a plain scalar.
func needsQuoting(v string) bool {
	return v == "" || strings.ContainsAny(v, ":#{}[],&*!|>'\"%@`\n") || strings.TrimSpace(v) != v
}

// equivalent reports whether out decodes to the same data as the edited tree.
func (f *yamlFile) equivalent(out []byte) bool {
	reparsed, err := parseYAMLFile(out)
	if err != nil || len(reparsed.docs) != len(f.docs) {
		return false
	}
	for i := range f.docs {
		var want, got interface{}
		if err := f.docs[i].Decode(&want); err != nil {
			return false
		}
		if err := reparsed.docs[i].Decode(&got); err != nil {
			return false
		}
		if !reflect.DeepEqual(want, got) {
			return false
		}
	}
	return true
}
