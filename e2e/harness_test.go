// Package e2e runs the real `gatehouse serve` binary against fake providers,
// a fake detector plugin and a fake OTLP collector, and writes Markdown
// reports to e2e/report/. Run with: go test ./e2e/ -v
package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	binDir       string
	gatehouse    string
	fakeDetPath  string
	fakePlugPath string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gatehouse-e2e-")
	if err != nil {
		panic(err)
	}
	binDir = dir
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	gatehouse = filepath.Join(dir, "gatehouse"+exe)
	fakeDetPath = filepath.Join(dir, "fakedetector"+exe)
	fakePlugPath = filepath.Join(dir, "fakeplugin"+exe)
	for out, pkg := range map[string]string{gatehouse: "../cmd/gatehouse", fakeDetPath: "./fakedetector", fakePlugPath: "./fakeplugin"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			panic(fmt.Sprintf("build %s: %v", pkg, err))
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// ---------- fake OpenAI-compatible provider ----------

// FakeProvider is an OpenAI-compatible HTTP server that records every request.
type FakeProvider struct {
	Name string
	URL  string
	ln   net.Listener
	srv  *http.Server

	mu          sync.Mutex
	requests    [][]byte
	authHeaders []string
	down        bool
	// Classify answers classifier prompts (system prompt mentions 路由分类器).
	Classify func(userText string) string
	// RedirectTo, when set, answers requests whose last user message
	// contains "#redirect" with a 307 to this URL.
	RedirectTo string
}

func startFake(t *testing.T, name string) *FakeProvider {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &FakeProvider{Name: name, URL: "http://" + ln.Addr().String() + "/v1", ln: ln}
	f.srv = &http.Server{Handler: http.HandlerFunc(f.serve)}
	go f.srv.Serve(ln)
	t.Cleanup(func() { f.srv.Close() })
	return f
}

// SetDown makes the provider answer 503 (simulating an outage).
func (f *FakeProvider) SetDown(v bool) { f.mu.Lock(); f.down = v; f.mu.Unlock() }

// Requests returns the raw bodies received so far (classifier calls included).
func (f *FakeProvider) Requests() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.requests...)
}

// AuthHeaders returns the Authorization header values received so far.
func (f *FakeProvider) AuthHeaders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authHeaders...)
}

// ChatRequests returns decoded non-classifier requests.
func (f *FakeProvider) ChatRequests() []map[string]any {
	var out []map[string]any
	for _, b := range f.Requests() {
		var m map[string]any
		json.Unmarshal(b, &m)
		if !isClassifier(m) {
			out = append(out, m)
		}
	}
	return out
}

// ClassifierCalls counts classifier requests.
func (f *FakeProvider) ClassifierCalls() int {
	n := 0
	for _, b := range f.Requests() {
		var m map[string]any
		json.Unmarshal(b, &m)
		if isClassifier(m) {
			n++
		}
	}
	return n
}

func isClassifier(m map[string]any) bool {
	ms, _ := m["messages"].([]any)
	if len(ms) == 0 {
		return false
	}
	first, _ := ms[0].(map[string]any)
	s, _ := first["content"].(string)
	return strings.Contains(s, "路由分类器")
}

func lastUser(m map[string]any) string {
	ms, _ := m["messages"].([]any)
	for i := len(ms) - 1; i >= 0; i-- {
		mo, _ := ms[i].(map[string]any)
		if mo["role"] == "user" {
			s, _ := mo["content"].(string)
			return s
		}
	}
	return ""
}

func (f *FakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.authHeaders = append(f.authHeaders, r.Header.Get("Authorization"))
	down := f.down
	f.mu.Unlock()
	if down {
		http.Error(w, `{"error":{"message":"service unavailable"}}`, http.StatusServiceUnavailable)
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	model, _ := req["model"].(string)
	user := lastUser(req)
	if strings.Contains(user, "#slow") {
		time.Sleep(1 * time.Second)
	}
	if f.RedirectTo != "" && strings.Contains(user, "#redirect") {
		http.Redirect(w, r, f.RedirectTo+"/chat/completions", http.StatusTemporaryRedirect)
		return
	}
	content := fmt.Sprintf("[%s/%s] %s", f.Name, model, user)
	if isClassifier(req) {
		content = "0"
		if f.Classify != nil {
			content = f.Classify(user)
		}
	}
	msg := map[string]any{"role": "assistant", "content": content}
	if strings.Contains(user, "#think") {
		msg["reasoning_content"] = "思考..."
	}
	finish := "stop"
	var toolArgsStr string
	hasTools := false
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		hasTools = true
		args, _ := json.Marshal(map[string]string{"echo": user})
		toolArgsStr = string(args)
		msg["tool_calls"] = []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "echo", "arguments": toolArgsStr}}}
		finish = "tool_calls"
	}
	promptTok := len(body) / 10
	compTok := len(content) / 4
	usage := map[string]any{
		"prompt_tokens":         promptTok,
		"completion_tokens":     compTok,
		"total_tokens":          promptTok + compTok,
		"prompt_tokens_details": map[string]any{"cached_tokens": promptTok / 2},
	}
	if stream, _ := req["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(user, "#truncate") {
			// one partial chunk, then a clean close with no finish or [DONE]
			b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "partial"}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
			return
		}
		if strings.Contains(user, "#think") {
			for _, part := range []string{"思考", "..."} {
				b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": part}}}})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
		}
		for _, part := range []string{content[:len(content)/2], content[len(content)/2:]} {
			b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}}})
			fmt.Fprintf(w, "data: %s\n\n", b)
		}
		if hasTools {
			mid := len(toolArgsStr) / 2
			chunks := []map[string]any{
				{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "echo", "arguments": ""}},
				{"index": 0, "function": map[string]any{"arguments": toolArgsStr[:mid]}},
				{"index": 0, "function": map[string]any{"arguments": toolArgsStr[mid:]}},
			}
			for _, tc := range chunks {
				b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model,
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{tc}}}}})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}
		}
		b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\n", b)
		b, _ = json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": model, "choices": []any{}, "usage": usage})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"id": "c1", "object": "chat.completion", "created": time.Now().Unix(), "model": model,
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   usage,
	})
}

// ---------- gateway process ----------

// Gateway is a running `gatehouse serve`.
type Gateway struct {
	Dir   string
	URL   string // http://127.0.0.1:port/v1
	Token string // local personal token
	cmd   *exec.Cmd
	log   *bytes.Buffer
}

func freePort(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// startGateway writes config (with {{LISTEN}} and {{DIR}} substituted) and starts serve.
func startGateway(t *testing.T, cfg string, files map[string]string) *Gateway {
	t.Helper()
	dir := t.TempDir()
	addr := freePort(t)
	cfg = strings.ReplaceAll(cfg, "{{LISTEN}}", addr)
	cfg = strings.ReplaceAll(cfg, "{{DIR}}", filepath.ToSlash(dir))
	cfg = strings.ReplaceAll(cfg, "{{FAKEDET}}", filepath.ToSlash(fakeDetPath))
	cfg = strings.ReplaceAll(cfg, "{{FAKEPLUGIN}}", filepath.ToSlash(fakePlugPath))
	for name, content := range files {
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &Gateway{Dir: dir, URL: "http://" + addr + "/v1", log: &bytes.Buffer{}}
	g.cmd = exec.Command(gatehouse, "serve", "--config", filepath.Join(dir, "config.yaml"))
	g.cmd.Stdout, g.cmd.Stderr = g.log, g.log
	if err := g.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Stop() })
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			b, _ := os.ReadFile(filepath.Join(dir, "local.token"))
			g.Token = strings.TrimSpace(string(b))
			return g
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("gateway did not start:\n%s", g.log.String())
	return nil
}

// Stop sends SIGTERM (flushes OTLP) and waits.
func (g *Gateway) Stop() {
	if g.cmd.ProcessState != nil {
		return
	}
	if runtime.GOOS == "windows" {
		g.cmd.Process.Kill()
	} else {
		g.cmd.Process.Signal(syscall.SIGTERM)
	}
	done := make(chan struct{})
	go func() { g.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		g.cmd.Process.Kill()
		<-done
	}
}

// CLI runs a gatehouse subcommand against this gateway's config.
func (g *Gateway) CLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := g.runCLI("", args...)
	if err != nil {
		t.Fatalf("gatehouse %v: %v\n%s", args, err, out)
	}
	return out
}

// CLIStdin runs a gatehouse subcommand with stdin input, returning combined output and error.
func (g *Gateway) CLIStdin(stdin string, args ...string) (string, error) {
	return g.runCLI(stdin, args...)
}

func (g *Gateway) runCLI(stdin string, args ...string) (string, error) {
	args = append(args, "--config", filepath.Join(g.Dir, "config.yaml"))
	// flags must precede positional args for the flag package; move --config after the subcommand words
	cmd := exec.Command(gatehouse, reorder(args)...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func reorder(args []string) []string {
	// subcommand words first, then flags, then positionals
	var words, flags, pos []string
	i := 0
	for i < len(args) && !strings.HasPrefix(args[i], "-") && len(words) < 2 && isWord(args[i]) {
		words = append(words, args[i])
		i++
	}
	for ; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !isBoolFlag(args[i]) {
				flags = append(flags, args[i+1])
				i++
			}
		} else {
			pos = append(pos, args[i])
		}
	}
	return append(append(words, flags...), pos...)
}

func isWord(s string) bool {
	switch s {
	case "token", "issue", "revoke", "list", "config", "check", "redact", "try", "report", "key", "set", "delete",
		"apikey", "create", "import", "update", "enable", "disable", "reset", "admin", "set-password":
		return true
	}
	return false
}

func isBoolFlag(s string) bool { return s == "--json" || s == "-json" }

// Response is a gateway reply.
type Response struct {
	Status int
	Header http.Header
	Body   map[string]any
	Raw    []byte
}

// Chat posts a chat completion.
func (g *Gateway) Chat(t *testing.T, token string, body map[string]any, headers map[string]string) Response {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, g.URL+"/chat/completions", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := Response{Status: resp.StatusCode, Header: resp.Header, Raw: raw}
	json.Unmarshal(raw, &out.Body)
	return out
}

// Content returns choices[0].message.content.
func (r Response) Content() string {
	ch, _ := r.Body["choices"].([]any)
	if len(ch) == 0 {
		return ""
	}
	msg, _ := ch[0].(map[string]any)["message"].(map[string]any)
	s, _ := msg["content"].(string)
	return s
}

// ToolArgs returns choices[0].message.tool_calls[0].function.arguments.
func (r Response) ToolArgs() string {
	ch, _ := r.Body["choices"].([]any)
	if len(ch) == 0 {
		return ""
	}
	msg, _ := ch[0].(map[string]any)["message"].(map[string]any)
	tcs, _ := msg["tool_calls"].([]any)
	if len(tcs) == 0 {
		return ""
	}
	fn, _ := tcs[0].(map[string]any)["function"].(map[string]any)
	s, _ := fn["arguments"].(string)
	return s
}

// ErrCode returns error.code.
func (r Response) ErrCode() string {
	e, _ := r.Body["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// AuditRecords reads every audit line.
func (g *Gateway) AuditRecords(t *testing.T) ([]map[string]any, string) {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(g.Dir, "audit", "*.jsonl"))
	var out []map[string]any
	var raw strings.Builder
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			raw.WriteString(sc.Text() + "\n")
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			out = append(out, m)
		}
		fh.Close()
	}
	return out, raw.String()
}

func msgs(pairs ...string) []any {
	var out []any
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, map[string]any{"role": pairs[i], "content": pairs[i+1]})
	}
	return out
}

// ---------- report ----------

// Report collects checks and writes e2e/report/<name>.md.
type Report struct {
	t     *testing.T
	name  string
	title string
	intro string
	rows  []string
	notes []string
	fail  bool
}

func newReport(t *testing.T, name, title, intro string) *Report {
	r := &Report{t: t, name: name, title: title, intro: intro}
	t.Cleanup(r.write)
	return r
}

// Check records one step; it fails the test when ok is false.
func (r *Report) Check(step string, ok bool, detail string, a ...any) {
	r.t.Helper()
	d := fmt.Sprintf(detail, a...)
	mark := "✅"
	if !ok {
		mark = "❌"
		r.fail = true
		r.t.Errorf("%s: %s", step, d)
	}
	r.rows = append(r.rows, fmt.Sprintf("| %s | %s | %s |", mdEscape(step), mark, mdEscape(d)))
}

// Note adds free text below the table.
func (r *Report) Note(format string, a ...any) { r.notes = append(r.notes, fmt.Sprintf(format, a...)) }

func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", `\|`)
	return strings.ReplaceAll(s, "\n", "<br>")
}

func (r *Report) write() {
	var b strings.Builder
	status := "通过"
	if r.fail || r.t.Failed() {
		status = "未通过"
	}
	fmt.Fprintf(&b, "# %s\n\n结果：**%s** · 运行于 %s · %s/%s\n\n%s\n\n| 步骤 | 结果 | 细节 |\n| --- | --- | --- |\n%s\n",
		r.title, status, time.Now().Format("2006-01-02 15:04:05 MST"), runtime.GOOS, runtime.GOARCH, r.intro, strings.Join(r.rows, "\n"))
	for _, n := range r.notes {
		b.WriteString("\n" + n + "\n")
	}
	os.MkdirAll("report", 0o755)
	if err := os.WriteFile(filepath.Join("report", r.name+".md"), []byte(b.String()), 0o644); err != nil {
		r.t.Logf("write report: %v", err)
	}
}

var placeholderRe = regexp.MustCompile(`\{\{[A-Z_]+_[a-z2-7]{8}\}\}`)

func header(r Response, k string) string { return r.Header.Get(k) }

func str(v any) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(v)
}
