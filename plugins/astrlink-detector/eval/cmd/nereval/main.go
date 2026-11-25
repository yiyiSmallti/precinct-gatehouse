// Command nereval evaluates AstrLink Guard on the bench/ner sample set and
// measures its latency.
//
//	nereval -ort <libonnxruntime> -model <dir> -mode eval  -data bench/ner/data -out bench/ner/results/eval-int8
//	nereval -ort <libonnxruntime> -model <dir> -mode perf  -data bench/ner/data
//	nereval -ort <libonnxruntime> -model <dir> -mode cold
//	nereval -ort <libonnxruntime> -model <dir> -mode tags  < texts.jsonl   (per-token tags, for ORT parity checks)
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/plugins/astrlink-detector/astrlink"
)

type entity struct {
	Label      string `json:"label"`
	Start, End int
	Text       string `json:"text"`
}

type segment struct {
	Role     string   `json:"role"`
	Text     string   `json:"text"`
	Entities []entity `json:"entities"`
}

type sample struct {
	ID       string    `json:"id"`
	Group    string    `json:"group"`
	Lang     string    `json:"lang"`
	Category string    `json:"category"`
	InFence  bool      `json:"in_fence"`
	Text     string    `json:"text"`
	Entities []entity  `json:"entities"`
	Segments []segment `json:"segments"`
}

func main() {
	ortLib := flag.String("ort", os.Getenv("ORT_LIB"), "path to libonnxruntime")
	modelDir := flag.String("model", os.Getenv("ASTRLINK_DIR"), "model folder")
	mode := flag.String("mode", "eval", "eval | perf | cold | tags")
	data := flag.String("data", "bench/ner/data", "sample folder")
	out := flag.String("out", "", "eval: output prefix for .json and .md")
	threads := flag.Int("threads", 0, "intra-op threads (0 = ORT default)")
	wordVote := flag.Bool("wordvote", false, "unify sub-word piece types within an ASCII word")
	minScore := flag.Float64("min", 0.8, "scan: minimum span score")
	flag.Parse()

	t0 := time.Now()
	if err := astrlink.InitRuntime(*ortLib); err != nil {
		log.Fatal(err)
	}
	tInit := time.Since(t0)
	opts := astrlink.DefaultOptions()
	opts.Threads = *threads
	opts.WordVote = *wordVote
	opts.MinScore = float32(*minScore)
	d, err := astrlink.Open(*modelDir, opts)
	if err != nil {
		log.Fatal(err)
	}
	tLoad := time.Since(t0)
	switch *mode {
	case "cold":
		rssLoad := rssMB()
		t1 := time.Now()
		if _, err := d.Detect("请联系张伟，邮箱 zhangwei@example.com。"); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("{\"init_runtime_ms\":%.1f,\"load_total_ms\":%.1f,\"first_detect_ms\":%.1f,\"rss_after_load_mb\":%.1f,\"rss_after_first_mb\":%.1f}\n",
			ms(tInit), ms(tLoad), ms(time.Since(t1)), rssLoad, rssMB())
	case "tags":
		runTags(d)
	case "scan":
		runScan(d, flag.Args())
	case "perf":
		runPerf(d, loadAll(*data))
	default:
		runEval(d, loadAll(*data), *out)
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// rssMB is the current resident set size (ps reports KiB).
func rssMB() float64 {
	b, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	kb, _ := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return kb / 1024
}

func maxRSSMB() float64 {
	var ru syscall.Rusage
	_ = syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	if runtime.GOOS == "darwin" {
		return float64(ru.Maxrss) / (1 << 20)
	}
	return float64(ru.Maxrss) / 1024
}

func loadAll(dir string) []sample {
	var all []sample
	for _, name := range []string{"positives.jsonl", "negatives.jsonl", "requests.jsonl"} {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			log.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var s sample
			if err := json.Unmarshal(sc.Bytes(), &s); err != nil {
				log.Fatal(err)
			}
			if s.Segments == nil {
				s.Segments = []segment{{Role: "user", Text: s.Text, Entities: s.Entities}}
			}
			all = append(all, s)
		}
		f.Close()
	}
	return all
}

// ---------------------------------------------------------------- tags

func runTags(d *astrlink.Detector) {
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	enc := json.NewEncoder(os.Stdout)
	for sc.Scan() {
		var in struct{ ID, Text string }
		if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
			log.Fatal(err)
		}
		toks := d.Tok.Encode(in.Text)
		w := []int32{astrlink.BOS}
		for _, t := range toks {
			w = append(w, t.ID)
		}
		w = append(w, astrlink.EOS)
		lg, err := d.Model.Run([][]int32{w})
		if err != nil {
			log.Fatal(err)
		}
		_ = enc.Encode(map[string]any{"id": in.ID, "logits": lg[0]})
	}
}

// ---------------------------------------------------------------- eval

var targets = []string{"private_person", "private_address", "account", "common_secret", "private_date"}
var thresholds = []float32{0.7, 0.8, 0.9, 0.95}
var langs = []string{"zh", "en", "mixed", "all"}

type pred struct {
	sample, seg int
	astrlink.Span
	Text string
}

type counts struct{ TP, FP, FN int }

func (c counts) prf() (p, r, f float64) {
	if c.TP+c.FP > 0 {
		p = float64(c.TP) / float64(c.TP+c.FP)
	}
	if c.TP+c.FN > 0 {
		r = float64(c.TP) / float64(c.TP+c.FN)
	}
	if p+r > 0 {
		f = 2 * p * r / (p + r)
	}
	return
}

type cell struct {
	Strict, Overlap counts
}

type result struct {
	Scope     string  `json:"scope"`
	Threshold float32 `json:"threshold"`
	// [lang][label] (label "macro" for the five-type macro F1 is computed at render time)
	Cells map[string]map[string]*cell `json:"cells"`
	// gold entities covered by a span of any type (what masking needs)
	Covered, Gold map[string]int
	// in-fence gold entities: found (overlap, same label) / total
	FenceFound, FenceGold int
	// clean units with at least one span: [lang] -> {flagged, total}
	// keyed by lang and by group ("negative", "request")
	CleanAny, CleanNoURLIP, CleanTotal map[string]int
	// tokens scanned in clean units and spans on them other than url/ip
	CleanTokens, CleanSpansNoURLIP map[string]int
	// spans on clean units by label
	CleanFPByLabel map[string]int
	// predictions in positive samples whose label is not one of the five
	OtherLabels map[string]int
	ScanMS      float64
}

func runEval(d *astrlink.Detector, samples []sample, outPrefix string) {
	var results []*result
	examples := map[string][]string{}
	for _, scope := range []string{"full", "skip_fenced"} {
		d.Opts.SkipFenced = scope == "skip_fenced"
		// one pass per scope at threshold 0; thresholds filter afterwards
		preds := make([][][]pred, len(samples))
		ntok := make([][]int, len(samples))
		t0 := time.Now()
		for i, s := range samples {
			preds[i] = make([][]pred, len(s.Segments))
			ntok[i] = make([]int, len(s.Segments))
			for k, seg := range s.Segments {
				ntok[i][k] = len(d.Tok.Encode(seg.Text))
				spans, err := d.DetectAll(seg.Text)
				if err != nil {
					log.Fatal(err)
				}
				rs := []rune(seg.Text)
				for _, sp := range spans {
					preds[i][k] = append(preds[i][k], pred{i, k, sp, string(rs[sp.Start:sp.End])})
				}
			}
		}
		scan := time.Since(t0)
		for _, th := range thresholds {
			r := score(samples, preds, ntok, th)
			r.Scope, r.Threshold, r.ScanMS = scope, th, ms(scan)
			results = append(results, r)
			if th == 0.8 {
				examples[scope] = collectExamples(samples, preds, th)
			}
		}
	}
	md := render(results, examples)
	fmt.Print(md)
	if outPrefix != "" {
		_ = os.MkdirAll(filepath.Dir(outPrefix), 0o755)
		b, _ := json.MarshalIndent(map[string]any{"results": results, "examples": examples}, "", " ")
		if err := os.WriteFile(outPrefix+".json", b, 0o644); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(outPrefix+".md", []byte(md), 0o644); err != nil {
			log.Fatal(err)
		}
	}
}

func isTarget(l string) bool {
	for _, t := range targets {
		if t == l {
			return true
		}
	}
	return false
}

func score(samples []sample, preds [][][]pred, ntok [][]int, th float32) *result {
	r := &result{Cells: map[string]map[string]*cell{}, Covered: map[string]int{}, Gold: map[string]int{},
		CleanAny: map[string]int{}, CleanNoURLIP: map[string]int{}, CleanTotal: map[string]int{},
		CleanFPByLabel: map[string]int{}, OtherLabels: map[string]int{}, CleanTokens: map[string]int{}, CleanSpansNoURLIP: map[string]int{}}
	get := func(lang, label string) *cell {
		if r.Cells[lang] == nil {
			r.Cells[lang] = map[string]*cell{}
		}
		if r.Cells[lang][label] == nil {
			r.Cells[lang][label] = &cell{}
		}
		return r.Cells[lang][label]
	}
	for i, s := range samples {
		clean := len(s.Segments) > 0
		for _, seg := range s.Segments {
			if len(seg.Entities) > 0 {
				clean = false
			}
		}
		anyFlag, anyFlagNoURL := false, false
		spansNoURL, toks := 0, 0
		for k, seg := range s.Segments {
			toks += ntok[i][k]
			var ps []pred
			for _, p := range preds[i][k] {
				if p.Score >= th {
					ps = append(ps, p)
				}
			}
			for _, p := range ps {
				anyFlag = true
				if p.Label != "url" && p.Label != "ip_address" {
					anyFlagNoURL = true
					spansNoURL++
				}
			}
			usedS := make([]bool, len(ps))
			usedO := make([]bool, len(ps))
			for _, g := range seg.Entities {
				for _, lang := range []string{s.Lang, "all"} {
					r.Gold[lang]++
				}
				strict, overlap, covered := false, false, false
				for j, p := range ps {
					ov := p.Start < g.End && g.Start < p.End
					covered = covered || ov
					if p.Label != g.Label || !ov {
						continue
					}
					if !overlap && !usedO[j] {
						overlap, usedO[j] = true, true
					}
					if !strict && !usedS[j] && p.Start == g.Start && p.End == g.End {
						strict, usedS[j] = true, true
					}
				}
				for _, lang := range []string{s.Lang, "all"} {
					c := get(lang, g.Label)
					if strict {
						c.Strict.TP++
					} else {
						c.Strict.FN++
					}
					if overlap {
						c.Overlap.TP++
					} else {
						c.Overlap.FN++
					}
					if covered {
						r.Covered[lang]++
					}
				}
				if s.InFence {
					r.FenceGold++
					if overlap {
						r.FenceFound++
					}
				}
			}
			for j, p := range ps {
				if !isTarget(p.Label) {
					if s.Group == "positive" {
						r.OtherLabels[p.Label]++
					}
					continue
				}
				for _, lang := range []string{s.Lang, "all"} {
					c := get(lang, p.Label)
					if !usedS[j] {
						c.Strict.FP++
					}
					if !usedO[j] {
						// a span inside a gold entity of the same label that was
						// already matched is a duplicate, not a false positive
						dup := false
						for _, g := range seg.Entities {
							if g.Label == p.Label && p.Start < g.End && g.Start < p.End {
								dup = true
							}
						}
						if !dup {
							c.Overlap.FP++
						}
					}
				}
			}
			if clean {
				for _, p := range ps {
					r.CleanFPByLabel[p.Label]++
				}
			}
		}
		if clean {
			for _, lang := range []string{s.Lang, "all", s.Group} {
				r.CleanTotal[lang]++
				r.CleanTokens[lang] += toks
				r.CleanSpansNoURLIP[lang] += spansNoURL
				if anyFlag {
					r.CleanAny[lang]++
				}
				if anyFlagNoURL {
					r.CleanNoURLIP[lang]++
				}
			}
		}
	}
	return r
}

func excerpt(text string, s, e int) string {
	rs := []rune(text)
	a, b := max(0, s-18), min(len(rs), e+18)
	x := string(rs[a:s]) + "【" + string(rs[s:e]) + "】" + string(rs[e:b])
	return strings.NewReplacer("\n", "⏎", "\t", " ", "|", "\\|").Replace(x)
}

func collectExamples(samples []sample, preds [][][]pred, th float32) []string {
	var fps, fns []string
	for i, s := range samples {
		for k, seg := range s.Segments {
			for _, p := range preds[i][k] {
				if p.Score < th {
					continue
				}
				hit := false
				for _, g := range seg.Entities {
					if g.Label == p.Label && p.Start < g.End && g.Start < p.End {
						hit = true
					}
				}
				if !hit {
					fps = append(fps, fmt.Sprintf("FP | %s | %s | %.3f | %s", s.ID, p.Label, p.Score, excerpt(seg.Text, p.Start, p.End)))
				}
			}
			for _, g := range seg.Entities {
				hit := false
				var near []string
				for _, p := range preds[i][k] {
					if p.Start < g.End && g.Start < p.End {
						near = append(near, fmt.Sprintf("%s %.2f「%s」", p.Label, p.Score, p.Text))
						if p.Label == g.Label && p.Score >= th {
							hit = true
						}
					}
				}
				if !hit {
					fns = append(fns, fmt.Sprintf("FN | %s | %s | %s | %s", s.ID, g.Label, excerpt(seg.Text, g.Start, g.End), strings.Join(near, "; ")))
				}
			}
		}
	}
	return append(fps, fns...)
}

func pct(a, b int) string {
	if b == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%% (%d/%d)", 100*float64(a)/float64(b), a, b)
}

func render(results []*result, examples map[string][]string) string {
	var b strings.Builder
	for _, r := range results {
		fmt.Fprintf(&b, "\n### scope=%s threshold=%.2f (scan %.0f ms)\n\n", r.Scope, r.Threshold, r.ScanMS)
		b.WriteString("| lang | label | gold | strict P | strict R | strict F1 | overlap P | overlap R | overlap F1 |\n|---|---|---|---|---|---|---|---|---|\n")
		for _, lang := range langs {
			var macroS, macroO float64
			for _, l := range targets {
				c := r.Cells[lang][l]
				if c == nil {
					c = &cell{}
				}
				sp, sr, sf := c.Strict.prf()
				op, or, of := c.Overlap.prf()
				macroS += sf / float64(len(targets))
				macroO += of / float64(len(targets))
				fmt.Fprintf(&b, "| %s | %s | %d | %.3f | %.3f | %.3f | %.3f | %.3f | %.3f |\n", lang, l, c.Strict.TP+c.Strict.FN, sp, sr, sf, op, or, of)
			}
			fmt.Fprintf(&b, "| %s | **macro F1** | | | | **%.3f** | | | **%.3f** |\n", lang, macroS, macroO)
		}
		b.WriteString("\n| unit | clean units flagged (any label) | flagged excl. url/ip | spans excl. url/ip per 10k clean tokens | gold covered by any span |\n|---|---|---|---|---|\n")
		for _, lang := range []string{"zh", "en", "mixed", "all", "negative", "request"} {
			dens := "-"
			if r.CleanTokens[lang] > 0 {
				dens = fmt.Sprintf("%.2f (%d spans / %d tokens)", 1e4*float64(r.CleanSpansNoURLIP[lang])/float64(r.CleanTokens[lang]), r.CleanSpansNoURLIP[lang], r.CleanTokens[lang])
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", lang, pct(r.CleanAny[lang], r.CleanTotal[lang]), pct(r.CleanNoURLIP[lang], r.CleanTotal[lang]), dens, pct(r.Covered[lang], r.Gold[lang]))
		}
		fmt.Fprintf(&b, "\nin-fence gold found: %s; spans on clean units by label: %v; non-target labels in positives: %v\n",
			pct(r.FenceFound, r.FenceGold), sortedMap(r.CleanFPByLabel), sortedMap(r.OtherLabels))
	}
	for _, scope := range []string{"full", "skip_fenced"} {
		fmt.Fprintf(&b, "\n### examples scope=%s threshold=0.80 (FP: label, score, 【span】; FN: gold, overlapping spans)\n\n", scope)
		for _, e := range examples[scope] {
			b.WriteString("- " + e + "\n")
		}
	}
	return b.String()
}

func sortedMap(m map[string]int) string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var parts []string
	for _, k := range ks {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return "{" + strings.Join(parts, " ") + "}"
}

// ---------------------------------------------------------------- scan

// runScan detects in each file (at -min) and prints every span with context,
// then the totals: files, tokens, spans by label.
func runScan(d *astrlink.Detector, files []string) {
	byLabel := map[string]int{}
	tokens, spans := 0, 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			log.Fatal(err)
		}
		text := string(b)
		tokens += len(d.Tok.Encode(text))
		ss, err := d.Detect(text)
		if err != nil {
			log.Fatal(err)
		}
		rs := []rune(text)
		for _, s := range ss {
			spans++
			byLabel[s.Label]++
			fmt.Printf("%s | %s | %.3f | %s\n", f, s.Label, s.Score, excerpt(string(rs), s.Start, s.End))
		}
	}
	fmt.Printf("files=%d tokens=%d spans=%d per10k=%.2f by_label=%s\n", len(files), tokens, spans, 1e4*float64(spans)/float64(max(tokens, 1)), sortedMap(byLabel))
}

// ---------------------------------------------------------------- perf

func runPerf(d *astrlink.Detector, samples []sample) {
	// corpus: every sample text in order, repeated as needed
	var corpus []string
	for _, s := range samples {
		for _, seg := range s.Segments {
			corpus = append(corpus, seg.Text)
		}
	}
	build := func(tokens int) string {
		var sb strings.Builder
		n := 0
		for i := 0; n < tokens; i++ {
			t := corpus[i%len(corpus)]
			sb.WriteString(t)
			sb.WriteString("\n\n")
			n += len(d.Tok.Encode(t))
		}
		return sb.String()
	}
	// one full 512 window
	text := build(600)
	toks := d.Tok.Encode(text)[:510]
	win := []int32{astrlink.BOS}
	for _, t := range toks {
		win = append(win, t.ID)
	}
	win = append(win, astrlink.EOS)
	report := map[string]any{"cpus": runtime.NumCPU()}
	for i := 0; i < 5; i++ { // warm-up
		if _, err := d.Model.Run([][]int32{win}); err != nil {
			log.Fatal(err)
		}
	}
	lat := make([]float64, 0, 200)
	for i := 0; i < 200; i++ {
		t0 := time.Now()
		if _, err := d.Model.Run([][]int32{win}); err != nil {
			log.Fatal(err)
		}
		lat = append(lat, ms(time.Since(t0)))
	}
	sort.Float64s(lat)
	report["window512_p50_ms"] = lat[len(lat)/2]
	report["window512_p95_ms"] = lat[len(lat)*95/100]
	report["window512_max_ms"] = lat[len(lat)-1]
	for _, bs := range []int{4, 8} {
		batch := make([][]int32, bs)
		for i := range batch {
			batch[i] = win
		}
		t0 := time.Now()
		const reps = 15
		for i := 0; i < reps; i++ {
			if _, err := d.Model.Run(batch); err != nil {
				log.Fatal(err)
			}
		}
		report[fmt.Sprintf("batch%d_ms_per_window", bs)] = ms(time.Since(t0)) / float64(reps*bs)
	}
	// end-to-end Detect on prompts of a given token count (tokenize + windows + decode)
	for _, n := range []int{2000, 8000, 32000, 131072} {
		t := build(n)
		t0 := time.Now()
		nt := len(d.Tok.Encode(t))
		tokMS := ms(time.Since(t0))
		t1 := time.Now()
		if _, err := d.Detect(t); err != nil {
			log.Fatal(err)
		}
		report[fmt.Sprintf("detect_%dk", n/1000)] = map[string]any{"tokens": nt, "tokenize_ms": tokMS, "detect_ms": ms(time.Since(t1))}
	}
	report["rss_mb"] = rssMB()
	report["max_rss_mb"] = maxRSSMB()
	b, _ := json.MarshalIndent(report, "", " ")
	fmt.Println(string(b))
}
