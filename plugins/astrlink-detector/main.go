// Command astrlink-detector is a Gatehouse Detector plugin (go-plugin gRPC
// subprocess) that runs the AstrLink Guard NER model with ONNX Runtime. It
// is a separate module because it needs cgo; the gateway itself stays
// CGO-free. See README.md for files, flags and the evaluation.
//
// The plugin maps model labels to finding kinds and reports every span
// scored at least -min with its score; per-category thresholds, the action
// cap and timeout behaviour are set in gatehouse.yaml:
//
//	plugins:
//	  - name: astrlink
//	    kind: detector
//	    path: /opt/gatehouse/plugins/astrlink-detector
//	    args: ["-model", "/opt/gatehouse/models/astrlink-int8", "-ort", "/opt/gatehouse/lib/libonnxruntime.so.1.27.0"]
//	redaction:
//	  detectors: [astrlink]
//	  sources:
//	    astrlink:
//	      min_confidence: { default: 0.8, person: 0.9, date: 0.9, account: 0.7 }
//	      max_action: mask
//	      timeout: 10s
//	      on_error: rules_only
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/yiyiSmallti/precinct-gatehouse/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/plugins/astrlink-detector/astrlink"
)

// defaultKinds maps AstrLink labels to Gatehouse finding kinds. Kinds that
// the rule engine also produces with a stricter meaning get an NER_ prefix so
// that a category action such as `secret: block` never applies to a model
// guess. "-" drops the label: URLs and IP addresses are left to the rules
// (in coding traffic the model mostly flags loopback addresses and doc links).
const defaultKinds = "private_person=PERSON,private_address=ADDRESS,account=ACCOUNT,common_secret=NER_SECRET," +
	"private_date=DATE,phone=PHONE,email=EMAIL,payment_card=NER_CARD,ip_address=-,url=-"

type detector struct {
	d     *astrlink.Detector
	kinds map[string]string
	min   float32
	sem   chan struct{}
}

func (x *detector) Detect(ctx context.Context, text, _ string, _ map[string]string) ([]plugin.Finding, error) {
	select {
	case x.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-x.sem }()
	spans, err := x.d.DetectAllContext(ctx, text)
	if err != nil {
		return nil, err
	}
	var out []plugin.Finding
	for _, s := range spans {
		kind, ok := x.kinds[s.Label]
		if !ok || kind == "-" {
			continue
		}
		if s.Score < x.min {
			continue
		}
		out = append(out, plugin.Finding{Kind: kind, Start: s.Start, End: s.End, Confidence: s.Score})
	}
	return out, nil
}

func parseMap(s string) (map[string]string, error) {
	m := map[string]string{}
	for _, kv := range strings.Split(s, ",") {
		if kv = strings.TrimSpace(kv); kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("bad pair %q", kv)
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, nil
}

func main() {
	model := flag.String("model", os.Getenv("ASTRLINK_DIR"), "model folder (model_int8.onnx or model.onnx, tokenizer.json, config.json)")
	ortLib := flag.String("ort", os.Getenv("ORT_LIB"), "ONNX Runtime shared library (>= 1.26)")
	minScore := flag.Float64("min", 0.5, "floor: spans scored below it are not reported (set per-category thresholds in redaction.sources.<name>.min_confidence)")
	kinds := flag.String("kinds", defaultKinds, "label=KIND,... (KIND '-' drops the label)")
	skipFenced := flag.Bool("skip-fenced", false, "do not scan ``` fenced code blocks")
	threads := flag.Int("threads", 4, "ONNX Runtime intra-op threads per inference (0 = all cores)")
	batch := flag.Int("batch", 4, "512-token windows per inference call")
	concurrency := flag.Int("concurrency", 1, "Detect calls running inference at the same time")
	wordVote := flag.Bool("wordvote", true, "unify the type of sub-word pieces of one ASCII word")
	flag.Parse()

	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "astrlink-detector:", err)
		os.Exit(1)
	}
	km, err := parseMap(*kinds)
	if err != nil {
		fail(err)
	}
	// Load before serving: go-plugin's handshake waits, and a model that
	// fails to load makes the plugin fail to start (the host then treats the
	// detector as missing and fails closed).
	if err := astrlink.InitRuntime(*ortLib); err != nil {
		fail(err)
	}
	opts := astrlink.DefaultOptions()
	opts.SkipFenced, opts.Threads, opts.Batch, opts.WordVote = *skipFenced, *threads, *batch, *wordVote
	opts.MinScore = 0 // the floor is applied in Detect
	d, err := astrlink.Open(*model, opts)
	if err != nil {
		fail(err)
	}
	plugin.ServeDetector(&detector{d: d, kinds: km, min: float32(*minScore), sem: make(chan struct{}, max(1, *concurrency))})
}
