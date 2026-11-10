package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// tenants.yml is hand-edited, so any field can hold the wrong type and the
// validator must name that field instead of aborting the decode. Leaf values
// therefore stay yaml.Node and are judged through the accessors below; once
// validated they are copied into the plain structs of validate.go.

const (
	tagStr   = "!!str"
	tagInt   = "!!int"
	tagFloat = "!!float"
	tagBool  = "!!bool"
	tagNull  = "!!null"
)

// quotedOrBlock are the scalar styles that make a value a string no matter how
// it looks.
const quotedOrBlock = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle |
	yaml.LiteralStyle | yaml.FoldedStyle | yaml.TaggedStyle

// yaml11Bools are the plain scalars PyYAML (YAML 1.1) reads as booleans but
// yaml.v3 (YAML 1.2) keeps as strings. The Python generator rejected
// `incus_project: no`; so must this one.
var yaml11Bools = map[string]bool{
	"yes": true, "Yes": true, "YES": true, "no": false, "No": false, "NO": false,
	"on": true, "On": true, "ON": true, "off": false, "Off": false, "OFF": false,
}

// decimalIntRe is the only integer spelling accepted: YAML 1.1 and 1.2 disagree
// on 010, 0o10, 0x10 and 1_0, and a number that renders differently depending
// on the reader must not reach an env file.
var decimalIntRe = regexp.MustCompile(`^[+-]?(0|[1-9][0-9]*)$`)

// errNotMapping marks a value that was expected to be a YAML mapping.
var errNotMapping = errors.New("not a mapping")

// follow resolves an alias to the node it points at.
func follow(n *yaml.Node) *yaml.Node {
	if n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		return n.Alias
	}
	return n
}

// present reports whether the key exists at all (explicit null included).
func present(n *yaml.Node) bool { return n.Kind != 0 }

// isNull is true for an absent key and for an explicit null: Python's `is None`.
func isNull(n *yaml.Node) bool {
	r := follow(n)
	return r.Kind == 0 || (r.Kind == yaml.ScalarNode && r.ShortTag() == tagNull)
}

// yaml11Bool reports whether n is a plain scalar PyYAML would have read as a
// boolean, and which one.
func yaml11Bool(n *yaml.Node) (value, ok bool) {
	if n.Kind != yaml.ScalarNode || n.Style&quotedOrBlock != 0 {
		return false, false
	}
	value, ok = yaml11Bools[n.Value]
	return value, ok
}

// stringOf returns the value of a YAML string scalar; ok is false for every
// other type (numbers, booleans, null, collections, absent keys).
func stringOf(n *yaml.Node) (s string, ok bool) {
	r := follow(n)
	if r.Kind != yaml.ScalarNode || r.ShortTag() != tagStr {
		return "", false
	}
	if _, ambiguous := yaml11Bool(r); ambiguous {
		return "", false
	}
	return r.Value, true
}

// decimalInt returns an integer scalar written in plain decimal that fits int64.
func decimalInt(n *yaml.Node) (int64, bool) {
	r := follow(n)
	if r.Kind != yaml.ScalarNode || r.ShortTag() != tagInt || !decimalIntRe.MatchString(r.Value) {
		return 0, false
	}
	v, err := strconv.ParseInt(r.Value, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// itemsOf returns the elements of a YAML sequence.
func itemsOf(n *yaml.Node) ([]*yaml.Node, bool) {
	r := follow(n)
	if r.Kind != yaml.SequenceNode {
		return nil, false
	}
	return r.Content, true
}

// decodeRow decodes a YAML mapping into T. Keys T does not name are ignored, as
// the Python generator ignored fields it does not consume. A value that is not
// a mapping yields errNotMapping.
func decodeRow[T any](n *yaml.Node) (*T, error) {
	r := follow(n)
	if r.Kind != yaml.MappingNode {
		return nil, errNotMapping
	}
	var row T
	if err := r.Decode(&row); err != nil {
		return nil, err
	}
	return &row, nil
}

// reprBudget bounds the text one value may add to an error message: aliases can
// expand a few lines of YAML into gigabytes.
const reprBudget = 200

// repr renders a value the way Python's repr() showed it in the generator's
// error messages, which drift.sh pastes verbatim into its report.
func repr(n *yaml.Node) string {
	var b strings.Builder
	writeRepr(&b, n, 0)
	return b.String()
}

func writeRepr(b *strings.Builder, n *yaml.Node, depth int) {
	if b.Len() > reprBudget || depth > 4 {
		b.WriteString("...")
		return
	}
	r := follow(n)
	switch r.Kind {
	case 0:
		b.WriteString("None")
	case yaml.ScalarNode:
		b.WriteString(scalarRepr(r))
	case yaml.SequenceNode:
		b.WriteByte('[')
		for i, item := range r.Content {
			if i > 0 {
				b.WriteString(", ")
			}
			if b.Len() > reprBudget {
				b.WriteString("...")
				break
			}
			writeRepr(b, item, depth+1)
		}
		b.WriteByte(']')
	case yaml.MappingNode:
		b.WriteByte('{')
		for i := 0; i+1 < len(r.Content); i += 2 {
			if i > 0 {
				b.WriteString(", ")
			}
			if b.Len() > reprBudget {
				b.WriteString("...")
				break
			}
			writeRepr(b, r.Content[i], depth+1)
			b.WriteString(": ")
			writeRepr(b, r.Content[i+1], depth+1)
		}
		b.WriteByte('}')
	case yaml.DocumentNode, yaml.AliasNode:
		b.WriteString("...")
	}
}

func scalarRepr(r *yaml.Node) string {
	if v, ok := yaml11Bool(r); ok {
		return pyBool(v)
	}
	switch r.ShortTag() {
	case tagNull:
		return "None"
	case tagBool:
		return pyBool(strings.EqualFold(r.Value, "true"))
	case tagStr:
		return pyQuote(r.Value)
	case tagInt, tagFloat:
		return r.Value
	default:
		return pyQuote(r.Value)
	}
}

func pyBool(v bool) string {
	if v {
		return "True"
	}
	return "False"
}

// pyQuote is Python's repr(str): single quotes unless the text holds a single
// quote and no double quote, with the same escapes.
func pyQuote(s string) string {
	quote := '\''
	if strings.ContainsRune(s, '\'') && !strings.ContainsRune(s, '"') {
		quote = '"'
	}
	var b strings.Builder
	b.WriteRune(quote)
	for _, r := range s {
		switch {
		case r == quote || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteRune(quote)
	return b.String()
}

// pyStrList renders a list of strings as Python's repr(list).
func pyStrList(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = pyQuote(s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}
