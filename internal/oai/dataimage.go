package oai

import "strings"

// DataImageSpans returns the byte spans [start, end) of every
// `data:image/<subtype>;base64,<payload>` URI in s whose base64 payload is at
// least minPayload bytes long. With allowSpace, ASCII whitespace inside the
// payload (line-wrapped base64) is part of the span.
//
// It is a hand-written scanner rather than a regexp so that a multi-megabyte
// string costs one strings.Index pass plus one byte walk per URI.
func DataImageSpans(s string, minPayload int, allowSpace bool) [][2]int {
	const prefix = "data:image/"
	const marker = ";base64,"
	var spans [][2]int
	for off := 0; off < len(s); {
		i := strings.Index(s[off:], prefix)
		if i < 0 {
			break
		}
		start := off + i
		p := start + len(prefix)
		for p < len(s) && isMimeByte(s[p]) {
			p++
		}
		if p == start+len(prefix) || !strings.HasPrefix(s[p:], marker) {
			off = start + len(prefix)
			continue
		}
		p += len(marker)
		payloadStart, payload := p, 0
		for p < len(s) {
			c := s[p]
			if isBase64Byte(c) {
				payload++
			} else if !(allowSpace && isSpaceByte(c)) {
				break
			}
			p++
		}
		end := p
		if allowSpace {
			// trailing whitespace belongs to the surrounding text
			for end > payloadStart && isSpaceByte(s[end-1]) {
				end--
			}
		}
		if payload >= minPayload {
			spans = append(spans, [2]int{start, end})
		}
		off = max(p, start+len(prefix))
	}
	return spans
}

func isMimeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-'
}

func isBase64Byte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '='
}

func isSpaceByte(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
