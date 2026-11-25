// Package astrlink runs the AstrLink Guard token-classification model
// (QuantumNous/astrlink-guard) from Go: an XLM-R Unigram tokenizer that reads
// the model's tokenizer.json, ONNX Runtime inference over 512-token windows,
// and BIO decoding into code point spans.
package astrlink

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

// Token is one tokenizer output. Start/End are Unicode code point offsets
// into the input text, matching what HF tokenizers reports in Python.
type Token struct {
	ID         int32
	Start, End int
	Special    bool
	Space      bool // the piece starts with ▁ (begins a pre-token word)
}

// Special token ids of the XLM-R vocabulary.
const (
	BOS = 0 // <s>
	PAD = 1 // <pad>
	EOS = 2 // </s>
)

// Tokenizer reproduces the tokenizer.json pipeline of the model:
// added tokens → Precompiled (SentencePiece nmt_nfkc charsmap) →
// Split(`(?<![0-9\s])\s+|^\s+`, removed) → Split(`\s`, isolated) →
// Metaspace(▁, prepend always, split) → Unigram (fuse unk).
type Tokenizer struct {
	vocab     map[string]int32
	scores    []float64
	unkID     int32
	unkScore  float64
	maxRunes  int
	charsmap  *precompiled
	added     []addedToken
	VocabSize int
}

type addedToken struct {
	content string
	id      int32
	lstrip  bool
	rstrip  bool
}

const unkPenalty = 10.0

// LoadTokenizer reads a HF tokenizer.json with a Unigram model.
func LoadTokenizer(path string) (*Tokenizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var tj struct {
		AddedTokens []struct {
			Content string `json:"content"`
			ID      int32  `json:"id"`
			Lstrip  bool   `json:"lstrip"`
			Rstrip  bool   `json:"rstrip"`
			Special bool   `json:"special"`
		} `json:"added_tokens"`
		Normalizer struct {
			Type        string `json:"type"`
			Normalizers []struct {
				Type    string `json:"type"`
				Charmap string `json:"precompiled_charsmap"`
			} `json:"normalizers"`
		} `json:"normalizer"`
		Model struct {
			Type  string               `json:"type"`
			UnkID int32                `json:"unk_id"`
			Vocab [][2]json.RawMessage `json:"vocab"`
		} `json:"model"`
	}
	if err := json.Unmarshal(b, &tj); err != nil {
		return nil, err
	}
	if tj.Model.Type != "Unigram" {
		return nil, fmt.Errorf("tokenizer model %q, want Unigram", tj.Model.Type)
	}
	t := &Tokenizer{vocab: make(map[string]int32, len(tj.Model.Vocab)), unkID: tj.Model.UnkID, VocabSize: len(tj.Model.Vocab)}
	t.scores = make([]float64, len(tj.Model.Vocab))
	minScore := 0.0
	for i, v := range tj.Model.Vocab {
		var piece string
		var score float64
		if err := json.Unmarshal(v[0], &piece); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(v[1], &score); err != nil {
			return nil, err
		}
		t.vocab[piece] = int32(i)
		t.scores[i] = score
		minScore = min(minScore, score)
		t.maxRunes = max(t.maxRunes, utf8.RuneCountInString(piece))
	}
	t.unkScore = minScore - unkPenalty
	for _, n := range tj.Normalizer.Normalizers {
		if n.Type == "Precompiled" {
			raw, err := base64.StdEncoding.DecodeString(n.Charmap)
			if err != nil {
				return nil, fmt.Errorf("precompiled_charsmap: %w", err)
			}
			if t.charsmap, err = newPrecompiled(raw); err != nil {
				return nil, err
			}
		}
	}
	if t.charsmap == nil {
		return nil, fmt.Errorf("tokenizer has no Precompiled normalizer")
	}
	for _, a := range tj.AddedTokens {
		if a.Special {
			t.added = append(t.added, addedToken{a.Content, a.ID, a.Lstrip, a.Rstrip})
		}
	}
	return t, nil
}

// Encode tokenizes text without adding <s> and </s>.
func (t *Tokenizer) Encode(text string) []Token {
	runes := []rune(text)
	var out []Token
	last := 0
	for _, m := range t.findAdded(runes) {
		if m.start > last {
			out = t.encodeSegment(runes, last, m.start, out)
		}
		out = append(out, Token{ID: m.id, Start: m.start, End: m.end, Special: true})
		last = m.end
	}
	if last < len(runes) {
		out = t.encodeSegment(runes, last, len(runes), out)
	}
	return out
}

type addedMatch struct {
	start, end int
	id         int32
}

// findAdded finds special tokens in the raw text (they are not normalized),
// leftmost-longest, with lstrip/rstrip absorbing neighboring whitespace.
func (t *Tokenizer) findAdded(runes []rune) []addedMatch {
	var ms []addedMatch
	s := string(runes)
	if !strings.Contains(s, "<") {
		return nil
	}
	for i := 0; i < len(runes); i++ {
		if runes[i] != '<' {
			continue
		}
		best := -1
		for k, a := range t.added {
			ar := []rune(a.content)
			if i+len(ar) <= len(runes) && string(runes[i:i+len(ar)]) == a.content && (best < 0 || len(ar) > len([]rune(t.added[best].content))) {
				best = k
			}
		}
		if best < 0 {
			continue
		}
		a := t.added[best]
		start, end := i, i+utf8.RuneCountInString(a.content)
		if a.lstrip {
			lo := 0
			if len(ms) > 0 {
				lo = ms[len(ms)-1].end
			}
			for start > lo && unicode.IsSpace(runes[start-1]) {
				start--
			}
		}
		if a.rstrip {
			for end < len(runes) && unicode.IsSpace(runes[end]) {
				end++
			}
		}
		ms = append(ms, addedMatch{start, end, a.id})
		i = end - 1
	}
	return ms
}

// span is a normalized rune with the original code point range it came from.
type span struct{ s, e int }

func (t *Tokenizer) encodeSegment(all []rune, from, to int, out []Token) []Token {
	norm, align := t.charsmap.normalize(all[from:to], from)
	// Pre-tokenize: drop whitespace runs per `(?<![0-9\s])\s+|^\s+`
	// (Oniguruma, Ruby syntax: ^ is a line start), then isolate each
	// remaining whitespace rune.
	n := len(norm)
	removed := make([]bool, n)
	for i := 0; i < n; {
		if !isSpace(norm[i]) {
			i++
			continue
		}
		j := i
		for j < n && isSpace(norm[j]) {
			j++
		}
		if i == 0 || !isDigit(norm[i-1]) {
			for k := i; k < j; k++ {
				removed[k] = true
			}
		} else {
			for k := i + 1; k < j; k++ {
				if norm[k-1] == '\n' {
					for m := k; m < j; m++ {
						removed[m] = true
					}
					break
				}
			}
		}
		i = j
	}
	for i := 0; i < n; {
		if removed[i] {
			i++
			continue
		}
		j := i + 1
		if !isSpace(norm[i]) {
			for j < n && !removed[j] && !isSpace(norm[j]) {
				j++
			}
		}
		out = t.metaspace(norm[i:j], align[i:j], out)
		i = j
	}
	return out
}

const meta = '▁'

// metaspace replaces ' ' with ▁, prepends ▁ (aligned to the first rune), and
// splits before every ▁; each piece goes to the Unigram model.
func (t *Tokenizer) metaspace(pre []rune, align []span, out []Token) []Token {
	rs := make([]rune, 0, len(pre)+1)
	al := make([]span, 0, len(pre)+1)
	if pre[0] != ' ' && pre[0] != meta {
		rs = append(rs, meta)
		al = append(al, align[0])
	}
	for i, r := range pre {
		if r == ' ' {
			r = meta
		}
		rs = append(rs, r)
		al = append(al, align[i])
	}
	start := 0
	for i := 1; i <= len(rs); i++ {
		if i == len(rs) || rs[i] == meta {
			out = t.unigram(rs[start:i], al[start:i], out)
			start = i
		}
	}
	return out
}

// unigram is the Viterbi search of HF tokenizers' Unigram::encode_optimized
// (ties keep the first, i.e. shorter, candidate; consecutive unknowns fuse).
func (t *Tokenizer) unigram(rs []rune, al []span, out []Token) []Token {
	n := len(rs)
	type node struct {
		score float64
		from  int
		id    int32
		set   bool
	}
	best := make([]node, n+1)
	best[0].set = true
	var sb strings.Builder
	for i := 0; i < n; i++ {
		if !best[i].set {
			continue
		}
		base := best[i].score
		single := false
		sb.Reset()
		for l := 1; l <= t.maxRunes && i+l <= n; l++ {
			sb.WriteRune(rs[i+l-1])
			id, ok := t.vocab[sb.String()]
			if !ok {
				continue
			}
			c := base + t.scores[id]
			if nd := &best[i+l]; !nd.set || c > nd.score {
				*nd = node{c, i, id, true}
			}
			if l == 1 {
				single = true
			}
		}
		if !single {
			c := base + t.unkScore
			if nd := &best[i+1]; !nd.set || c > nd.score {
				*nd = node{c, i, t.unkID, true}
			}
		}
	}
	// Walk back from the end; an unknown right after (in text order) another
	// unknown extends it leftwards instead of making a new token.
	var rev []Token
	for end := n; end > 0; {
		nd := best[end]
		tok := Token{ID: nd.id, Start: al[nd.from].s, End: al[end-1].e, Space: rs[nd.from] == meta}
		if nd.id == t.unkID && len(rev) > 0 && rev[len(rev)-1].ID == t.unkID {
			rev[len(rev)-1].Start = tok.Start
		} else {
			rev = append(rev, tok)
		}
		end = nd.from
	}
	for i := len(rev) - 1; i >= 0; i-- {
		out = append(out, rev[i])
	}
	return out
}

func isSpace(r rune) bool { return unicode.IsSpace(r) }
func isDigit(r rune) bool { return r >= '0' && r <= '9' }

// ---------------------------------------------------------------- charsmap

// precompiled is SentencePiece's precompiled charsmap: a darts-clone double
// array over UTF-8 keys whose values index NUL-terminated replacements.
type precompiled struct {
	array      []uint32
	normalized []byte
}

func newPrecompiled(b []byte) (*precompiled, error) {
	if len(b) < 4 {
		return nil, fmt.Errorf("charsmap too short")
	}
	size := int(binary.LittleEndian.Uint32(b))
	if 4+size > len(b) {
		return nil, fmt.Errorf("charsmap trie size %d exceeds %d bytes", size, len(b))
	}
	p := &precompiled{array: make([]uint32, size/4), normalized: b[4+size:]}
	for i := range p.array {
		p.array[i] = binary.LittleEndian.Uint32(b[4+4*i:])
	}
	return p, nil
}

func dHasLeaf(u uint32) bool  { return (u>>8)&1 == 1 }
func dValue(u uint32) uint32  { return u & ((1 << 31) - 1) }
func dLabel(u uint32) uint32  { return u & ((1 << 31) | 0xFF) }
func dOffset(u uint32) uint32 { return (u >> 10) << ((u & (1 << 9)) >> 6) }

// transform returns the replacement for the shortest charsmap key that is a
// prefix of key (spm_precompiled uses results[0] of common_prefix_search).
func (p *precompiled) transform(key string) (string, bool) {
	pos := uint32(0)
	u := p.array[pos]
	pos ^= dOffset(u)
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == 0 {
			break
		}
		pos ^= uint32(c)
		if int(pos) >= len(p.array) {
			return "", false
		}
		u = p.array[pos]
		if dLabel(u) != uint32(c) {
			return "", false
		}
		pos ^= dOffset(u)
		if dHasLeaf(u) {
			if int(pos) >= len(p.array) {
				return "", false
			}
			v := int(dValue(p.array[pos]))
			end := v
			for end < len(p.normalized) && p.normalized[end] != 0 {
				end++
			}
			return string(p.normalized[v:end]), true
		}
	}
	return "", false
}

// change is one entry of HF's transformation list: an output rune and how it
// consumes the original (0: replaces one rune, 1: inserted, -k: replaces one
// rune and removes k more after it).
type change struct {
	r  rune
	cs int
}

// normalize applies the charsmap per extended grapheme cluster exactly like
// tokenizers' Precompiled normalizer, and aligns each output rune to the
// original code point range (offset base `base`) the way NormalizedString
// does.
//
// One deliberate difference: runes the charsmap deletes before any output
// rune (control characters at the start) have no previous entry to carry the
// removal, and HF drops them, shifting every later offset left. The gateway
// masks the original text by these offsets, so a shift would leave the end
// of an entity unmasked; they are counted in lead instead.
func (p *precompiled) normalize(in []rune, base int) ([]rune, []span) {
	var tr []change
	lead := 0 // original runes deleted before the first output rune
	replace := func(oldN int, repl string) {
		newN := 0
		for _, r := range repl {
			tr = append(tr, change{r, 0})
			newN++
		}
		switch d := newN - oldN; {
		case d > 0:
			for i := len(tr) - d; i < len(tr); i++ {
				tr[i].cs = 1
			}
		case d < 0:
			if len(tr) > 0 {
				tr[len(tr)-1].cs += d
			} else {
				lead -= d
			}
		}
	}
	g := uniseg.NewGraphemes(string(in))
	for g.Next() {
		cl := g.Str()
		rs := g.Runes()
		if len(cl) < 6 {
			if repl, ok := p.transform(cl); ok {
				replace(len(rs), repl)
				continue
			}
		}
		for _, r := range rs {
			if repl, ok := p.transform(string(r)); ok {
				replace(1, repl)
			} else {
				tr = append(tr, change{r, 0})
			}
		}
	}
	out := make([]rune, 0, len(tr))
	al := make([]span, 0, len(tr))
	o := lead // original rune cursor
	for _, c := range tr {
		var a span
		if c.cs > 0 {
			a = span{base + o - 1, base + o}
			if o == 0 {
				a = span{base, base}
			}
		} else {
			a = span{base + o, base + o + 1}
			o += 1 - c.cs
		}
		a.s, a.e = min(a.s, base+len(in)), min(a.e, base+len(in))
		out = append(out, c.r)
		al = append(al, a)
	}
	return out, al
}
