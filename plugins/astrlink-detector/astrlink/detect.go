package astrlink

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Options tune detection.
type Options struct {
	MinScore   float32 // drop spans scoring below this (model card: 0.8)
	SkipFenced bool    // do not scan ``` / ~~~ fenced code blocks
	Window     int     // tokens per window incl. <s> and </s> (512)
	Overlap    int     // tokens shared by neighboring windows (128)
	Batch      int     // windows per ONNX Runtime call
	Threads    int     // intra-op threads, 0 = ORT default
	WordVote   bool    // unify the type of sub-word pieces of one ASCII word
}

// DefaultOptions follow the model card.
func DefaultOptions() Options {
	return Options{MinScore: 0.8, Window: 512, Overlap: 128, Batch: 8}
}

// Span is one detected entity; Start/End are code point offsets, End
// exclusive. Score is the lowest token probability inside the span.
type Span struct {
	Label      string
	Start, End int
	Score      float32
}

// Detector is a tokenizer plus model plus decoding settings.
type Detector struct {
	Tok    *Tokenizer
	Model  *Model
	Labels []string // id -> BIO tag, e.g. "B-private_person"
	Opts   Options
}

// Open loads a model folder (model_int8.onnx or model.onnx, tokenizer.json,
// config.json) after InitRuntime.
func Open(dir string, opts Options) (*Detector, error) {
	tok, err := LoadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	labels, err := loadLabels(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, err
	}
	model := ""
	for _, name := range []string{"model_int8.onnx", "model.onnx"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			model = filepath.Join(dir, name)
			break
		}
	}
	if model == "" {
		return nil, fmt.Errorf("%s: no model_int8.onnx or model.onnx", dir)
	}
	m, err := LoadModel(model, len(labels), opts.Threads)
	if err != nil {
		return nil, err
	}
	return &Detector{Tok: tok, Model: m, Labels: labels, Opts: opts}, nil
}

func loadLabels(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c struct {
		ID2Label map[string]string `json:"id2label"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	labels := make([]string, len(c.ID2Label))
	for k, v := range c.ID2Label {
		i, err := strconv.Atoi(k)
		if err != nil || i < 0 || i >= len(labels) {
			return nil, fmt.Errorf("bad id2label key %q", k)
		}
		labels[i] = v
	}
	return labels, nil
}

// Detect returns the spans scoring at least Opts.MinScore.
func (d *Detector) Detect(text string) ([]Span, error) {
	return d.DetectContext(context.Background(), text)
}

// DetectContext is Detect that stops between model batches once ctx is done.
func (d *Detector) DetectContext(ctx context.Context, text string) ([]Span, error) {
	all, err := d.DetectAllContext(ctx, text)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, s := range all {
		if s.Score >= d.Opts.MinScore {
			out = append(out, s)
		}
	}
	return out, nil
}

// DetectAll returns every decoded span regardless of score, so callers can
// apply several thresholds to one pass.
func (d *Detector) DetectAll(text string) ([]Span, error) {
	return d.DetectAllContext(context.Background(), text)
}

// DetectAllContext is DetectAll with cancellation.
func (d *Detector) DetectAllContext(ctx context.Context, text string) ([]Span, error) {
	runes := []rune(text)
	if !d.Opts.SkipFenced {
		return d.detectRunes(ctx, runes, 0)
	}
	var out []Span
	for _, seg := range Unfenced(runes) {
		ss, err := d.detectRunes(ctx, runes[seg[0]:seg[1]], seg[0])
		if err != nil {
			return nil, err
		}
		out = append(out, ss...)
	}
	return out, nil
}

// Unfenced returns the [start,end) code point ranges outside fenced code
// blocks (lines opening with ``` or ~~~ after at most 3 spaces; an unclosed
// fence runs to the end).
func Unfenced(runes []rune) [][2]int {
	var segs [][2]int
	segStart, inFence := 0, false
	var marker string
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] != '\n' {
			j++
		}
		line := strings.TrimLeft(string(runes[i:j]), " ")
		indent := (j - i) - len([]rune(line))
		isFence := indent <= 3 && (strings.HasPrefix(line, "```") || strings.HasPrefix(line, "~~~"))
		if isFence && !inFence {
			if i > segStart {
				segs = append(segs, [2]int{segStart, i})
			}
			inFence, marker = true, line[:3]
		} else if isFence && inFence && strings.HasPrefix(line, marker) && strings.Trim(line, marker[:1]+" \t\r") == "" {
			inFence = false
			segStart = min(j+1, len(runes))
		}
		i = j + 1
	}
	if !inFence && segStart < len(runes) {
		segs = append(segs, [2]int{segStart, len(runes)})
	}
	return segs
}

func (d *Detector) detectRunes(ctx context.Context, runes []rune, base int) ([]Span, error) {
	toks := d.Tok.Encode(string(runes))
	if len(toks) == 0 {
		return nil, nil
	}
	// tokenizing a long text takes a while; a caller that has given up
	// should not keep the inference slot for the passes below
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	content := d.Opts.Window - 2
	step := content - d.Opts.Overlap
	var starts []int
	for s := 0; ; s += step {
		starts = append(starts, s)
		if s+content >= len(toks) {
			break
		}
	}
	// each token is decoded from the window where it sits farthest from an
	// edge (the earlier window on a tie). Walking each window's own tokens
	// keeps this linear: a token lies in at most content/step windows.
	owner := make([]int, len(toks))
	bestD := make([]int, len(toks))
	for t := range bestD {
		bestD[t] = -1
	}
	for w, s := range starts {
		for t := s; t < min(s+content, len(toks)); t++ {
			if dd := min(t-s, s+content-1-t); dd > bestD[t] {
				owner[t], bestD[t] = w, dd
			}
		}
	}
	dist := make([][]float32, len(toks)) // softmax per token
	nl := d.Model.NumLabels
	for b := 0; b < len(starts); b += d.Opts.Batch {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		e := min(b+d.Opts.Batch, len(starts))
		wins := make([][]int32, 0, e-b)
		for _, s := range starts[b:e] {
			end := min(s+content, len(toks))
			w := make([]int32, 0, end-s+2)
			w = append(w, BOS)
			for _, t := range toks[s:end] {
				w = append(w, t.ID)
			}
			wins = append(wins, append(w, EOS))
		}
		logits, err := d.Model.Run(wins)
		if err != nil {
			return nil, err
		}
		for wi, s := range starts[b:e] {
			end := min(s+content, len(toks))
			for t := s; t < end; t++ {
				if owner[t] != b+wi {
					continue
				}
				dist[t] = softmax(logits[wi][(t-s+1)*nl : (t-s+2)*nl])
			}
		}
	}
	tags := make([]int, len(toks))
	probs := make([]float32, len(toks))
	for t, p := range dist {
		tags[t] = argmax(p)
		probs[t] = p[tags[t]]
	}
	if d.Opts.WordVote {
		d.wordVote(runes, toks, dist, tags, probs)
	}
	return d.decode(runes, base, toks, tags, probs), nil
}

func softmax(row []float32) []float32 {
	m := row[argmax(row)]
	out := make([]float32, len(row))
	var sum float64
	for i, v := range row {
		e := math.Exp(float64(v - m))
		out[i] = float32(e)
		sum += e
	}
	for i := range out {
		out[i] = float32(float64(out[i]) / sum)
	}
	return out
}

func argmax(p []float32) int {
	best := 0
	for i, v := range p {
		if v > p[best] {
			best = i
		}
	}
	return best
}

func isWordChar(r rune) bool {
	return r == '_' || r < utf8RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
}

const utf8RuneSelf = 0x80

// wordVote gives every sub-word piece of one ASCII word (pieces that touch,
// the later one not starting with ▁, word characters on both sides of the
// seam) the same entity type: the type with the largest mean B+I probability
// over the pieces, or O when O's mean is larger. The first piece keeps B or I
// from its own distribution, the rest get I; each piece's score becomes the
// mean B+I probability of the chosen type.
func (d *Detector) wordVote(runes []rune, toks []Token, dist [][]float32, tags []int, probs []float32) {
	kinds := map[string][2]int{} // kind -> {B id, I id}
	for id, l := range d.Labels {
		bio, kind, ok := strings.Cut(l, "-")
		if !ok {
			continue
		}
		k := kinds[kind]
		if bio == "B" {
			k[0] = id
		} else {
			k[1] = id
		}
		kinds[kind] = k
	}
	oID := 0
	for id, l := range d.Labels {
		if l == "O" {
			oID = id
		}
	}
	joins := func(i int) bool {
		a, b := toks[i-1], toks[i]
		return !a.Special && !b.Special && !b.Space && b.Start == a.End && a.End > a.Start &&
			b.End > b.Start && isWordChar(runes[a.End-1]) && isWordChar(runes[b.Start])
	}
	for i := 0; i < len(toks); {
		j := i + 1
		for j < len(toks) && joins(j) {
			j++
		}
		if j-i > 1 {
			n := float32(j - i)
			mean := func(id int) float32 {
				var s float32
				for t := i; t < j; t++ {
					s += dist[t][id]
				}
				return s / n
			}
			bestKind, bestMass := "", mean(oID)
			for kind, ids := range kinds {
				if m := mean(ids[0]) + mean(ids[1]); m > bestMass {
					bestKind, bestMass = kind, m
				}
			}
			for t := i; t < j; t++ {
				switch {
				case bestKind == "":
					tags[t] = oID
				case t == i && dist[t][kinds[bestKind][1]] > dist[t][kinds[bestKind][0]]:
					tags[t] = kinds[bestKind][1]
				case t == i:
					tags[t] = kinds[bestKind][0]
				default:
					tags[t] = kinds[bestKind][1]
				}
				probs[t] = bestMass
			}
		}
		i = j
	}
}

// decode turns per-token BIO tags into spans: B starts a span, I extends a
// span of the same type (an I without one starts a span), and spans of one
// type whose character ranges touch are merged so a word split into several
// sub-word pieces yields one span. Boundaries are then trimmed.
func (d *Detector) decode(runes []rune, base int, toks []Token, tags []int, probs []float32) []Span {
	var spans []Span
	var cur *Span
	for i, t := range toks {
		tag := d.Labels[tags[i]]
		if t.Special || tag == "O" {
			cur = nil
			continue
		}
		bio, kind, _ := strings.Cut(tag, "-")
		if bio == "B" || cur == nil || cur.Label != kind {
			if n := len(spans); n > 0 && spans[n-1].Label == kind && t.Start <= spans[n-1].End {
				cur = &spans[n-1] // touching piece of the same entity
			} else {
				spans = append(spans, Span{Label: kind, Start: t.Start, End: t.End, Score: probs[i]})
				cur = &spans[len(spans)-1]
				continue
			}
		}
		cur.End = max(cur.End, t.End)
		cur.Score = min(cur.Score, probs[i])
	}
	out := spans[:0]
	for _, s := range spans {
		s.Start, s.End = trim(runes, s.Label, s.Start, s.End)
		if s.Start >= s.End {
			continue
		}
		s.Start += base
		s.End += base
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

const edgePunct = "，。、；：！？“”‘’「」『』（）《》【】,;:\"'`()[]{}<>"

// trim removes whitespace and surrounding punctuation from span edges, a
// trailing ":port" from IP addresses and trailing sentence punctuation from
// URLs (the model card's IP/URL boundary cleanup is not published; this is
// our approximation).
func trim(r []rune, label string, s, e int) (int, int) {
	isEdge := func(c rune) bool {
		if unicode.IsSpace(c) {
			return true
		}
		if label == "common_secret" { // keys may legitimately end in = . ! etc.
			return strings.ContainsRune("，。、；：！？“”‘’「」『』（）《》【】\"'`", c)
		}
		return strings.ContainsRune(edgePunct, c)
	}
	for s < e && isEdge(r[s]) {
		s++
	}
	for e > s && isEdge(r[e-1]) {
		e--
	}
	switch label {
	case "url":
		for e > s && strings.ContainsRune(".,;:!?", r[e-1]) {
			e--
		}
	case "ip_address":
		if k := strings.LastIndexByte(string(r[s:e]), ':'); k > 0 && strings.Count(string(r[s:e]), ":") == 1 {
			e = s + len([]rune(string(r[s:e])[:k]))
		}
	}
	return s, e
}
