package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// jnode is one JSON value with its byte span; object members and array elements keep their positions.
type jnode struct {
	kind     byte // '{' '[' 's' (string) 'o' (other scalar)
	start    int
	end      int
	keys     []string
	keyStart []int
	vals     []*jnode
}

type jparser struct {
	b []byte
	i int
}

func parseJSONSpans(b []byte) (*jnode, error) {
	p := &jparser{b: b}
	p.ws()
	n, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(b) {
		return nil, errors.New("trailing data")
	}
	return n, nil
}

func (p *jparser) ws() {
	for p.i < len(p.b) && (p.b[p.i] == ' ' || p.b[p.i] == '\t' || p.b[p.i] == '\n' || p.b[p.i] == '\r') {
		p.i++
	}
}

func (p *jparser) str() (int, int, error) {
	s := p.i
	p.i++
	for p.i < len(p.b) {
		switch p.b[p.i] {
		case '\\':
			p.i += 2
			continue
		case '"':
			p.i++
			return s, p.i, nil
		}
		p.i++
	}
	return 0, 0, errors.New("unterminated string")
}

func (p *jparser) value(depth int) (*jnode, error) {
	if depth > 200 || p.i >= len(p.b) {
		return nil, errors.New("bad json")
	}
	s := p.i
	switch c := p.b[p.i]; c {
	case '"':
		_, e, err := p.str()
		if err != nil {
			return nil, err
		}
		return &jnode{kind: 's', start: s, end: e}, nil
	case '{', '[':
		n := &jnode{kind: c, start: s}
		closer := byte('}')
		if c == '[' {
			closer = ']'
		}
		p.i++
		p.ws()
		if p.i < len(p.b) && p.b[p.i] == closer {
			p.i++
			n.end = p.i
			return n, nil
		}
		for {
			p.ws()
			if c == '{' {
				if p.i >= len(p.b) || p.b[p.i] != '"' {
					return nil, errors.New("expected key")
				}
				ks, ke, err := p.str()
				if err != nil {
					return nil, err
				}
				var key string
				if json.Unmarshal(p.b[ks:ke], &key) != nil {
					return nil, errors.New("bad key")
				}
				p.ws()
				if p.i >= len(p.b) || p.b[p.i] != ':' {
					return nil, errors.New("expected colon")
				}
				p.i++
				p.ws()
				n.keys = append(n.keys, key)
				n.keyStart = append(n.keyStart, ks)
			}
			v, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, v)
			p.ws()
			if p.i >= len(p.b) {
				return nil, errors.New("unterminated")
			}
			if p.b[p.i] == ',' {
				p.i++
				continue
			}
			if p.b[p.i] == closer {
				p.i++
				n.end = p.i
				return n, nil
			}
			return nil, errors.New("expected , or close")
		}
	default:
		for p.i < len(p.b) && !strings.ContainsRune(" \t\r\n,]}", rune(p.b[p.i])) {
			p.i++
		}
		if p.i == s {
			return nil, errors.New("bad value")
		}
		tok := p.b[s:p.i]
		if !json.Valid(tok) {
			return nil, errors.New("bad scalar")
		}
		return &jnode{kind: 'o', start: s, end: p.i}, nil
	}
}

func (n *jnode) get(key string) *jnode {
	if n == nil || n.kind != '{' {
		return nil
	}
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

func (n *jnode) at(path []string) *jnode {
	for _, k := range path {
		if n = n.get(k); n == nil {
			return nil
		}
	}
	return n
}

func (n *jnode) str(b []byte) (string, bool) {
	if n == nil || n.kind != 's' {
		return "", false
	}
	var s string
	if json.Unmarshal(b[n.start:n.end], &s) != nil {
		return "", false
	}
	return s, true
}

func (n *jnode) strList(b []byte) ([]string, bool) {
	if n == nil {
		return nil, true
	}
	if n.kind != '[' {
		return nil, false
	}
	out := []string{}
	for _, v := range n.vals {
		s, ok := v.str(b)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

type kv struct {
	K string
	V any
}

// obj marshals its members in the order given, so a new entry reads like the hand-written ones.
type obj []kv

func jsonText(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func (o obj) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, e := range o {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonText(e.K))
		buf.WriteByte(':')
		buf.Write(jsonText(e.V))
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

func lineIndent(b []byte, pos int) string {
	s := bytes.LastIndexByte(b[:pos], '\n') + 1
	e := s
	for e < len(b) && (b[e] == ' ' || b[e] == '\t') {
		e++
	}
	return string(b[s:e])
}

func indentUnit(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimLeft(l, " \t")
		if t != "" && len(t) < len(l) {
			return l[:len(l)-len(t)]
		}
	}
	return "  "
}

// render writes v with the indent the surrounding lines use; a one-line container gets compact text.
func render(v any, childIndent string, multi bool) string {
	if !multi {
		return string(jsonText(v))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent(childIndent, "  ")
	_ = enc.Encode(v)
	return strings.TrimRight(buf.String(), "\n")
}

// layout reports whether a container spans lines and which indent its children use.
func layout(b []byte, n *jnode) (multi bool, child string) {
	unit := indentUnit(b)
	own := lineIndent(b, n.start)
	if len(n.vals) == 0 {
		inner := b[n.start+1 : n.end-1]
		return bytes.ContainsRune(inner, '\n') || bytes.ContainsRune(b[:n.start], '\n'), own + unit
	}
	first := n.vals[0].start
	if len(n.keyStart) > 0 {
		first = n.keyStart[0]
	}
	if !bytes.ContainsRune(b[n.start:first], '\n') {
		return false, ""
	}
	return true, lineIndent(b, first)
}

func splice(b []byte, from, to int, ins string) []byte {
	out := make([]byte, 0, len(b)-(to-from)+len(ins))
	out = append(out, b[:from]...)
	out = append(out, ins...)
	return append(out, b[to:]...)
}

// insertMember adds key: v to object n after its last member; an empty {} gains one indented member.
func insertMember(b []byte, n *jnode, key string, v any) []byte {
	multi, child := layout(b, n)
	member := string(jsonText(key)) + ": " + render(v, child, multi)
	if len(n.vals) == 0 {
		if !multi {
			return splice(b, n.start+1, n.end-1, member)
		}
		return splice(b, n.start+1, n.end-1, "\n"+child+member+"\n"+lineIndent(b, n.start))
	}
	last := n.vals[len(n.vals)-1].end
	if !multi {
		return splice(b, last, last, ", "+member)
	}
	return splice(b, last, last, ",\n"+child+member)
}

func insertElem(b []byte, n *jnode, v any) []byte {
	multi, child := layout(b, n)
	elem := render(v, child, multi)
	if len(n.vals) == 0 {
		if !multi {
			return splice(b, n.start+1, n.end-1, elem)
		}
		return splice(b, n.start+1, n.end-1, "\n"+child+elem+"\n"+lineIndent(b, n.start))
	}
	last := n.vals[len(n.vals)-1].end
	if !multi {
		return splice(b, last, last, ", "+elem)
	}
	return splice(b, last, last, ",\n"+child+elem)
}

// removeItem drops member or element i, reversing insertMember and insertElem byte for byte.
func removeItem(b []byte, n *jnode, i int) []byte {
	switch {
	case len(n.vals) == 1:
		return splice(b, n.start+1, n.end-1, "")
	case i > 0:
		return splice(b, n.vals[i-1].end, n.vals[i].end, "")
	}
	from := n.vals[0].start
	next := n.vals[1].start
	if n.kind == '{' {
		from, next = n.keyStart[0], n.keyStart[1]
	}
	return splice(b, from, next, "")
}

func emptyDoc(b []byte) bool { return len(bytes.TrimSpace(b)) == 0 }

// putAtPath adds leaf under path, creating the missing objects; created is the outermost member it made.
func putAtPath(b []byte, path []string, leaf any) (out []byte, created []string, err error) {
	root, err := parseJSONSpans(b)
	if err != nil {
		return nil, nil, err
	}
	if root.kind != '{' {
		return nil, nil, errors.New("root is not an object")
	}
	cur := root
	for i, k := range path {
		next := cur.get(k)
		if next == nil {
			var v any = leaf
			for j := len(path) - 1; j > i; j-- {
				v = obj{{path[j], v}}
			}
			out = insertMember(b, cur, k, v)
			created = append([]string{}, path[:i+1]...)
			break
		}
		if i == len(path)-1 {
			return nil, nil, errors.New("already present")
		}
		if next.kind != '{' {
			return nil, nil, fmt.Errorf("%s is not an object", k)
		}
		cur = next
	}
	if out == nil {
		return nil, nil, errors.New("no place for the entry")
	}
	if err := checkJSON(out, path); err != nil {
		return nil, nil, err
	}
	return out, created, nil
}

func checkJSON(after []byte, path []string) error {
	if !json.Valid(after) {
		return errors.New("result is not valid JSON")
	}
	if n, err := parseJSONSpans(after); err != nil || n.at(path) == nil {
		return errors.New("entry not found after the edit")
	}
	return nil
}

// pruneAfterRemove deletes containers left empty, from containerPath up to the member we created.
func pruneAfterRemove(b []byte, containerPath, created []string) ([]byte, error) {
	for depth := len(containerPath); len(created) > 0 && depth >= len(created); depth-- {
		root, err := parseJSONSpans(b)
		if err != nil {
			return nil, err
		}
		n := root.at(containerPath[:depth])
		parent := root.at(containerPath[:depth-1])
		if n == nil || len(n.vals) != 0 || parent == nil || parent.kind != '{' {
			break
		}
		for i, k := range parent.keys {
			if k == containerPath[depth-1] {
				b = removeItem(b, parent, i)
				break
			}
		}
	}
	return b, nil
}
