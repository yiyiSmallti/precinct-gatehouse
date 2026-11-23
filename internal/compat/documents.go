package compat

import (
	"archive/zip"
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
)

// Attached files (Anthropic `document` blocks, Responses `input_file` parts)
// reach Chat Completions providers as text: the gateway extracts what it can
// and otherwise sends a short notice. Raw base64 file data is never sent as
// text, since the model cannot decode it and tokenizes all of it.

const (
	mimePDF  = "application/pdf"
	mimeDOCX = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	// maxDocText caps the text taken from one document.
	maxDocText = 256 << 10
	// maxDocXML caps the decompressed word/document.xml read from a .docx.
	maxDocXML = 64 << 20
)

// docFile is an attached file as the client sent it.
type docFile struct {
	Name      string
	MediaType string
	Data      []byte // decoded file bytes; nil when only Ref is known
	Text      string // plain text the client already supplied
	Ref       string // URL or file id the gateway does not fetch
}

// documentText renders an attached file as text for the model.
func documentText(f docFile) string {
	label := f.Name
	if label == "" {
		label = "未命名文件"
	}
	if f.Text != "" {
		return "[文档：" + label + "]\n" + capDocText(f.Text)
	}
	if f.Data == nil {
		if f.Ref != "" {
			return fmt.Sprintf("[网关提示：收到外部文件引用 %s（%s），网关不会下载该文件；请提供文件内容或纯文本后再发送]", f.Ref, label)
		}
		return fmt.Sprintf("[网关提示：收到空文件 %s]", label)
	}
	kind := docKind(f)
	var text string
	var err error
	switch kind {
	case mimePDF:
		text, err = pdfText(f.Data)
	case mimeDOCX:
		text, err = docxText(f.Data)
	case "text":
		if utf8.Valid(f.Data) {
			text = string(f.Data)
		} else {
			err = fmt.Errorf("not UTF-8")
		}
	default:
		err = fmt.Errorf("unsupported type")
	}
	if err == nil && strings.TrimSpace(text) != "" {
		return "[文档：" + label + "]\n" + capDocText(text)
	}
	mt := f.MediaType
	if mt == "" {
		mt = "未知类型"
	}
	return fmt.Sprintf("[网关提示：收到未解析的二进制文档（%s，%s，%d 字节），请先提取纯文本或转为图片后再发送]", label, mt, len(f.Data))
}

// docKind classifies a file by media type, then by content, then by name.
func docKind(f docFile) string {
	mt := strings.ToLower(strings.TrimSpace(strings.Split(f.MediaType, ";")[0]))
	switch {
	case mt == mimePDF || bytes.HasPrefix(f.Data, []byte("%PDF-")):
		return mimePDF
	case mt == mimeDOCX || bytes.HasPrefix(f.Data, []byte("PK\x03\x04")) && strings.EqualFold(path.Ext(f.Name), ".docx"):
		return mimeDOCX
	case strings.HasPrefix(mt, "text/") || mt == "application/json" || mt == "application/xml" ||
		mt == "application/x-yaml" || mt == "application/yaml" || mt == "application/csv":
		return "text"
	}
	if bytes.HasPrefix(f.Data, []byte("PK\x03\x04")) {
		if zr, err := zip.NewReader(bytes.NewReader(f.Data), int64(len(f.Data))); err == nil {
			for _, zf := range zr.File {
				if zf.Name == "word/document.xml" {
					return mimeDOCX
				}
			}
		}
	}
	switch strings.ToLower(path.Ext(f.Name)) {
	case ".txt", ".md", ".csv", ".json", ".yaml", ".yml", ".xml", ".log":
		return "text"
	}
	return ""
}

func capDocText(s string) string {
	if len(s) <= maxDocText {
		return s
	}
	cut := maxDocText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n[网关提示：文档文本过长，已截断，原文共 %d 字节]", len(s))
}

// pdfText extracts the plain text of a PDF. Scanned or encrypted PDFs yield
// an error or no text.
func pdfText(data []byte) (text string, err error) {
	defer func() {
		// the parser panics on some malformed files
		if r := recover(); r != nil {
			err = fmt.Errorf("pdf: %v", r)
		}
	}()
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	rd, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxDocText+1))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// docxText extracts the paragraphs of word/document.xml.
func docxText(data []byte) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", err
	}
	var doc *zip.File
	for _, zf := range zr.File {
		if zf.Name == "word/document.xml" {
			doc = zf
			break
		}
	}
	if doc == nil {
		return "", fmt.Errorf("docx: word/document.xml missing")
	}
	rc, err := doc.Open()
	if err != nil {
		return "", err
	}
	defer rc.Close()
	dec := xml.NewDecoder(io.LimitReader(rc, maxDocXML))
	var b strings.Builder
	inText := false
	for b.Len() <= maxDocText {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "t":
				inText = true
			case "tab":
				b.WriteByte('\t')
			case "br", "cr":
				b.WriteByte('\n')
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "t":
				inText = false
			case "p":
				b.WriteByte('\n')
			}
		case xml.CharData:
			if inText {
				b.Write(t)
			}
		}
	}
	return b.String(), nil
}

// decodeBase64 accepts standard or URL-safe base64, padded or not.
func decodeBase64(s string) ([]byte, bool) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			return b, true
		}
	}
	return nil, false
}

// parseDataURI splits `data:<mime>;base64,<payload>`.
func parseDataURI(s string) (mime string, data []byte, ok bool) {
	rest, found := strings.CutPrefix(s, "data:")
	if !found {
		return "", nil, false
	}
	meta, payload, found := strings.Cut(rest, ",")
	if !found || !strings.HasSuffix(meta, ";base64") {
		return "", nil, false
	}
	data, ok = decodeBase64(payload)
	return strings.TrimSuffix(meta, ";base64"), data, ok
}

// anthropicDocument reads an Anthropic `document` block.
func anthropicDocument(blk map[string]any) docFile {
	f := docFile{Name: oai.Str(blk, "title")}
	src, _ := blk["source"].(map[string]any)
	f.MediaType = oai.Str(src, "media_type")
	switch oai.Str(src, "type") {
	case "base64":
		if b, ok := decodeBase64(oai.Str(src, "data")); ok {
			f.Data = b
		} else {
			f.Data = []byte{}
		}
	case "text":
		f.Text = oai.Str(src, "data")
	case "content":
		switch c := src["content"].(type) {
		case string:
			f.Text = c
		case []any:
			var parts []string
			for _, it := range c {
				if m, ok := it.(map[string]any); ok && oai.Str(m, "type") == "text" {
					parts = append(parts, oai.Str(m, "text"))
				}
			}
			f.Text = strings.Join(parts, "\n")
		}
	case "url":
		f.Ref = oai.Str(src, "url")
	case "file":
		f.Ref = oai.Str(src, "file_id")
	}
	return f
}

// responsesInputFile reads a Responses `input_file` part.
func responsesInputFile(p map[string]any) docFile {
	f := docFile{Name: oai.Str(p, "filename")}
	if fd := oai.Str(p, "file_data"); fd != "" {
		if mt, b, ok := parseDataURI(fd); ok {
			f.MediaType, f.Data = mt, b
		} else if b, ok := decodeBase64(fd); ok {
			f.Data = b
		} else {
			f.Data = []byte{}
		}
		return f
	}
	if u := oai.Str(p, "file_url"); u != "" {
		f.Ref = u
	} else {
		f.Ref = oai.Str(p, "file_id")
	}
	return f
}
