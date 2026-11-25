// Command tokdump tokenizes JSONL lines {"id","text"} from stdin with the Go
// tokenizer and writes {"id","ids","offsets"} (no <s>/</s>) for comparison
// with HF tokenizers (gen/tok_compare.py).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/yiyiSmallti/precinct-gatehouse/plugins/astrlink-detector/astrlink"
)

func main() {
	path := flag.String("tokenizer", "", "tokenizer.json")
	flag.Parse()
	tok, err := astrlink.LoadTokenizer(*path)
	if err != nil {
		log.Fatal(err)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	enc := json.NewEncoder(w)
	for sc.Scan() {
		var in struct{ ID, Text string }
		if err := json.Unmarshal(sc.Bytes(), &in); err != nil {
			log.Fatal(err)
		}
		toks := tok.Encode(in.Text)
		ids := make([]int32, len(toks))
		offs := make([][2]int, len(toks))
		for i, t := range toks {
			ids[i], offs[i] = t.ID, [2]int{t.Start, t.End}
		}
		if err := enc.Encode(map[string]any{"id": in.ID, "ids": ids, "offsets": offs}); err != nil {
			log.Fatal(err)
		}
	}
	if err := sc.Err(); err != nil {
		log.Fatal(err)
	}
}
