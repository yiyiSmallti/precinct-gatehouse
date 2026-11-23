package oai

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Some client tools read .docx/.pdf files as text and return the raw bytes:
// replacement characters and control bytes that waste context and lead the
// model into garbled output. GuardBinaryToolOutput swaps such tool output for
// a short notice.

const (
	binaryMinLen       = 8 << 10
	binaryReplRatio    = 0.05
	binaryControlRatio = 0.03
	binaryMagicRatio   = 0.01
	binaryPreview      = 200
)

// GuardBinaryToolOutput replaces the text of tool messages that look like a
// raw binary file and returns how many texts it replaced.
func GuardBinaryToolOutput(req Object) int {
	n := 0
	for _, m := range Messages(req) {
		if Str(m, "role") != "tool" {
			continue
		}
		switch c := m["content"].(type) {
		case string:
			if LooksBinary(c) {
				m["content"] = binaryNotice(c)
				n++
			}
		case []any:
			for _, p := range c {
				po, ok := p.(map[string]any)
				if !ok || Str(po, "type") != "text" {
					continue
				}
				if s := Str(po, "text"); LooksBinary(s) {
					po["text"] = binaryNotice(s)
					n++
				}
			}
		}
	}
	return n
}

// LooksBinary reports whether s (longer than 8 KiB) is mostly undecodable
// bytes: more than 5% U+FFFD, more than 3% control characters other than
// tab, newline, carriage return and ESC (colored logs), or a ZIP/PDF header
// followed by more than 1% of either.
func LooksBinary(s string) bool {
	if len(s) <= binaryMinLen {
		return false
	}
	var runes, repl, ctrl int
	for _, r := range s {
		runes++
		switch {
		case r == utf8.RuneError:
			repl++
		case r == '\t' || r == '\n' || r == '\r' || r == 0x1b:
		case r < 0x20 || r == 0x7f:
			ctrl++
		}
	}
	rf, cf := float64(repl)/float64(runes), float64(ctrl)/float64(runes)
	if rf > binaryReplRatio || cf > binaryControlRatio {
		return true
	}
	head := strings.TrimLeft(s, " \t\r\n\uFEFF")
	if strings.HasPrefix(head, "PK\x03\x04") || strings.HasPrefix(head, "%PDF-") {
		return rf+cf > binaryMagicRatio
	}
	return false
}

func binaryNotice(s string) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= binaryPreview {
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = '.'
		}
		b.WriteRune(r)
	}
	return fmt.Sprintf("[网关保护：该工具输出疑似原始二进制文件（如 .docx/.pdf，约 %d 字节），不可按纯文本阅读，请改用 python-docx / pypdf / pdftotext 等解析工具重新读取，或告知用户提供纯文本]\n开头摘要：%s", len(s), b.String())
}
