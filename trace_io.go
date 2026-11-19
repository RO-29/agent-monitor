package main

import (
	"bytes"
	"compress/flate"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// packMin is the length from which span IO text is kept flate-compressed in memory.
const packMin = 512

// packedText holds short text as is and long text compressed.
type packedText struct {
	s string
	z []byte
}

type packedIO struct {
	args, result packedText
	keepResult   bool
}

// spanIOMap maps span id to its args and result; values are immutable, so copies share them.
type spanIOMap map[string]packedIO

var (
	flateWriters = sync.Pool{New: func() any { w, _ := flate.NewWriter(nil, flate.BestSpeed); return w }}
	flateReaders = sync.Pool{New: func() any { return flate.NewReader(bytes.NewReader(nil)) }}
)

func packText(s string) packedText {
	if len(s) < packMin {
		return packedText{s: s}
	}
	var buf bytes.Buffer
	w := flateWriters.Get().(*flate.Writer)
	w.Reset(&buf)
	_, _ = io.WriteString(w, s)
	err := w.Close()
	flateWriters.Put(w)
	if err != nil || buf.Len() >= len(s) {
		return packedText{s: s}
	}
	return packedText{z: bytes.Clone(buf.Bytes())}
}

func (p packedText) String() string {
	if p.z == nil {
		return p.s
	}
	r := flateReaders.Get().(io.ReadCloser)
	defer flateReaders.Put(r)
	if r.(flate.Resetter).Reset(bytes.NewReader(p.z), nil) != nil {
		return ""
	}
	var sb bytes.Buffer
	sb.Grow(len(p.z) * 4)
	_, _ = io.Copy(&sb, r)
	return sb.String()
}

func (m spanIOMap) get(id string) SpanIO {
	p, ok := m[id]
	if !ok {
		return SpanIO{}
	}
	return SpanIO{Args: p.args.String(), Result: p.result.String()}
}

func (m spanIOMap) args(id string) string { return m[id].args.String() }

func (m spanIOMap) result(id string) string { return m[id].result.String() }

func (m spanIOMap) set(id string, v SpanIO) {
	m[id] = packedIO{args: packText(v.Args), result: packText(v.Result), keepResult: true}
}

func (m spanIOMap) setResult(id, result string) {
	p := m[id]
	p.result, p.keepResult = packText(result), true
	m[id] = p
}

// keepIO says which IO a cached trace holds: bash args that can ship or start codex, and results that can ship.
// Everything else is served by a full parse (spanIOFor); ioAll traces keep it all.
func (tr *SessionTrace) keepIO(sp *Span, args string) (keepArgs, keepResult bool) {
	if tr.ioAll {
		return true, true
	}
	if sp.Fam != "bash" {
		return false, false
	}
	hint := reShipHint.MatchString(args) || reShipHint.MatchString(sp.Res)
	return hint || strings.Contains(args, "codex"), hint
}

func (tr *SessionTrace) setIO(sp *Span, v SpanIO) {
	ka, kr := tr.keepIO(sp, v.Args)
	if !ka {
		return
	}
	p := packedIO{args: packText(v.Args), keepResult: kr}
	if kr {
		p.result = packText(v.Result)
	}
	tr.io[sp.ID] = p
}

func (tr *SessionTrace) setIOResult(sp *Span, result string) {
	if p, ok := tr.io[sp.ID]; ok && p.keepResult {
		p.result = packText(result)
		tr.io[sp.ID] = p
	}
}

// fullTraces keeps the last few traces parsed with every span's IO, for the span detail endpoint.
var fullTraces = struct {
	sync.Mutex
	ents []fullTraceEnt
	at   time.Time // last read
}{}

type fullTraceEnt struct {
	id          string
	size, mtime int64
	tr          *SessionTrace
}

const fullTracesMax = 2

// spanIOFor returns a span's args and result, parsing the transcript in full when the cached trace dropped them.
func spanIOFor(sess *Session, tr *SessionTrace, spanID string) SpanIO {
	if tr.ioAll || sess == nil || sess.TranscriptPath == "" {
		return tr.io.get(spanID)
	}
	st, err := os.Stat(sess.TranscriptPath)
	if err != nil {
		return tr.io.get(spanID)
	}
	size, mtime := st.Size(), st.ModTime().UnixMilli()
	fullTraces.Lock()
	defer fullTraces.Unlock()
	fullTraces.at = time.Now()
	for i, e := range fullTraces.ents {
		if e.id == sess.ID && e.size == size && e.mtime == mtime {
			fullTraces.ents = append(append([]fullTraceEnt{e}, fullTraces.ents[:i]...), fullTraces.ents[i+1:]...)
			return e.tr.io.get(spanID)
		}
	}
	var full *SessionTrace
	switch sess.Tool {
	case ToolClaude:
		p := newClaudeTraceParser(sess)
		p.tr.ioAll = true
		p.advance()
		full = p.tr
	case ToolCodex:
		full = buildCodexTraceIO(sess, true)
	}
	if full == nil {
		return tr.io.get(spanID)
	}
	fullTraces.ents = append([]fullTraceEnt{{id: sess.ID, size: size, mtime: mtime, tr: full}}, fullTraces.ents...)
	if len(fullTraces.ents) > fullTracesMax {
		fullTraces.ents = fullTraces.ents[:fullTracesMax]
	}
	return full.io.get(spanID)
}
