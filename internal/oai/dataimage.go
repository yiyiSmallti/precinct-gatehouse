package oai

import "strings"

// maxWrapLine is the longest base64 line DataImageSpans treats as line
// wrapping (MIME uses 76, PEM 64).
const maxWrapLine = 1024

// DataImageSpans returns the byte spans [start, end) of every
// `data:image/<subtype>;base64,<payload>` URI in s whose base64 payload is at
// least minPayload bytes long. With allowWrap, a payload wrapped into lines
// of equal length (LF or CRLF) is one span. A line break only continues the
// payload after a full, unpadded line, and a wrapped payload must decode to
// whole quanta, so text after the image (for example a phone number on the
// next line) never joins the span.
//
// It is a hand-written scanner rather than a regexp so that a multi-megabyte
// string costs one strings.Index pass plus one byte walk per URI.
func DataImageSpans(s string, minPayload int, allowWrap bool) [][2]int {
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
		end, payload := scanBase64(s, p, allowWrap)
		if payload >= minPayload {
			spans = append(spans, [2]int{start, end})
		}
		off = max(end, start+len(prefix))
	}
	return spans
}

// scanBase64 reads a base64 payload starting at p and returns its end and
// its length without line breaks.
func scanBase64(s string, p int, allowWrap bool) (end, payload int) {
	lineLen := 0 // length of the first line once a wrap is seen
	seg, padded := 0, false
	end = p
	// end and payload of the last full line, to fall back to
	lineEnd, linePayload := p, 0
	for p < len(s) {
		c := s[p]
		if isBase64Byte(c) {
			if padded && c != '=' {
				break
			}
			padded = padded || c == '='
			seg++
			payload++
			p++
			end = p
			continue
		}
		if !allowWrap || padded || seg == 0 {
			break
		}
		brk := 0
		if c == '\n' {
			brk = 1
		} else if c == '\r' && p+1 < len(s) && s[p+1] == '\n' {
			brk = 2
		}
		if brk == 0 || p+brk >= len(s) || !isBase64Byte(s[p+brk]) {
			break
		}
		if lineLen == 0 {
			if seg > maxWrapLine || seg%4 != 0 {
				break
			}
			lineLen = seg
		} else if seg != lineLen {
			break
		}
		lineEnd, linePayload = end, payload
		seg = 0
		p += brk
	}
	if lineLen > 0 && payload%4 != 0 {
		// the last "line" is not base64 that completes the image
		return lineEnd, linePayload
	}
	return end, payload
}

func isMimeByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '+' || c == '-'
}

func isBase64Byte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '='
}
