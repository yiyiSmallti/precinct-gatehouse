// Command fakedetector is a detector plugin for the end-to-end tests. It
// reports PERSON for a fixed list of names, the way a NER plugin would, each
// with a fixed score (孙七 gets NaN, as a broken model might). Text containing "#crash-detector" makes the process
// exit (G17); "#hang-detector" makes the call take 3 s (G18).
//
// With a directory as its first argument it appends one line per Detect call
// to <dir>/fakedetector.calls (the text's length in runes, no text), so a
// test can count calls (G18).
package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

var names = []struct {
	name  string
	score float32
}{{"张三", 0.99}, {"李四", 0.99}, {"王五", 0.99}, {"赵六", 0.6}, {"孙七", float32(math.NaN())}}

type det struct{ calls string }

func (d det) Detect(_ context.Context, text, _ string, _ map[string]string) ([]plugin.Finding, error) {
	if d.calls != "" {
		if f, err := os.OpenFile(d.calls, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			fmt.Fprintln(f, utf8.RuneCountInString(text))
			f.Close()
		}
	}
	if strings.Contains(text, "#crash-detector") {
		os.Exit(3)
	}
	if strings.Contains(text, "#hang-detector") {
		time.Sleep(3 * time.Second)
	}
	var out []plugin.Finding
	for _, n := range names {
		for off := 0; ; {
			i := strings.Index(text[off:], n.name)
			if i < 0 {
				break
			}
			b := off + i
			s := utf8.RuneCountInString(text[:b])
			out = append(out, plugin.Finding{Kind: "PERSON", Start: s, End: s + utf8.RuneCountInString(n.name), Confidence: n.score})
			off = b + len(n.name)
		}
	}
	return out, nil
}

func main() {
	var d det
	if len(os.Args) > 1 {
		d.calls = filepath.Join(os.Args[1], "fakedetector.calls")
	}
	plugin.ServeDetector(d)
}
