// Command fakedetector is a detector plugin for the end-to-end tests. It
// reports PERSON for a fixed list of names, the way a NER plugin would.
// Text containing "#crash-detector" makes the process exit (G17).
package main

import (
	"context"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/plugin"
)

var names = []string{"张三", "李四", "王五"}

type det struct{}

func (det) Detect(_ context.Context, text, _ string, _ map[string]string) ([]plugin.Finding, error) {
	if strings.Contains(text, "#crash-detector") {
		os.Exit(3)
	}
	var out []plugin.Finding
	for _, n := range names {
		for off := 0; ; {
			i := strings.Index(text[off:], n)
			if i < 0 {
				break
			}
			b := off + i
			s := utf8.RuneCountInString(text[:b])
			out = append(out, plugin.Finding{Kind: "PERSON", Start: s, End: s + utf8.RuneCountInString(n), Confidence: 0.99})
			off = b + len(n)
		}
	}
	return out, nil
}

func main() { plugin.ServeDetector(det{}) }
