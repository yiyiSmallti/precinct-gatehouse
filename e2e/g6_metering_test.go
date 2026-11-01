package e2e

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	colmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

const g6Config = `
listen: {{LISTEN}}
mode: personal
providers:
  - name: local
    base_url: %s
    target: local
    models: [small]
    pricing: { small: { input: 0.5, output: 1 } }
  - name: cloud
    base_url: %s
    target: external
    models: [strong]
    pricing: { strong: { input: 2, output: 8 } }
default_group: default
routing_groups:
  default:
    members: [cloud/strong, local/small]
redaction:
  mode_by_target: { local: audit, external: mask }
meter:
  otlp: { endpoint: %s, interval: 1s }
`

// fakeCollector is an OTLP/HTTP metrics receiver keeping the latest
// cumulative value per (metric, attribute set).
type fakeCollector struct {
	URL    string
	mu     sync.Mutex
	latest map[string]float64 // metric|attrs -> value
	tokens map[string]string  // attrs key -> token id
	posts  int
}

func startCollector(t *testing.T) *fakeCollector {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeCollector{URL: "http://" + ln.Addr().String(), latest: map[string]float64{}, tokens: map[string]string{}}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req colmetric.ExportMetricsServiceRequest
		if err := proto.Unmarshal(b, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		c.mu.Lock()
		c.posts++
		for _, rm := range req.ResourceMetrics {
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					sum := m.GetSum()
					if sum == nil {
						continue
					}
					for _, dp := range sum.DataPoints {
						key, tok := attrKey(dp.Attributes)
						v := dp.GetAsDouble()
						if _, ok := dp.Value.(interface{ GetAsInt() int64 }); ok || dp.GetAsInt() != 0 {
							v = float64(dp.GetAsInt()) + dp.GetAsDouble()
						}
						c.latest[m.Name+"|"+key] = v
						c.tokens[key] = tok
					}
				}
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&colmetric.ExportMetricsServiceResponse{})
		w.Write(out)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return c
}

func attrKey(kvs []*commonpb.KeyValue) (string, string) {
	var parts []string
	tok := ""
	for _, kv := range kvs {
		v := kv.Value.GetStringValue()
		if v == "" {
			v = fmt.Sprint(kv.Value.GetIntValue())
		}
		parts = append(parts, kv.Key+"="+v)
		if kv.Key == "gatehouse.token.id" {
			tok = v
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), tok
}

// byToken sums the latest cumulative values of a metric per token id.
func (c *fakeCollector) byToken(metric string) map[string]float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]float64{}
	for k, v := range c.latest {
		name, attrs, _ := strings.Cut(k, "|")
		if name == metric {
			out[c.tokens[attrs]] += v
		}
	}
	return out
}

func TestG6Metering(t *testing.T) {
	rep := newReport(t, "G6-metering", "G6 计量",
		"两把令牌在本地与外部模型上发请求（含一次外部宕机后的故障转移与一次分类器之外的直连），网关停止时冲刷 OTLP。用 `gatehouse report --format json` 的按令牌汇总与假 OTLP 收集器收到的累计值逐项核对。")
	local := startFake(t, "local")
	cloud := startFake(t, "cloud")
	col := startCollector(t)
	g := startGateway(t, fmt.Sprintf(g6Config, local.URL, cloud.URL, col.URL), nil)
	issue := func(name string) string {
		out := g.CLI(t, "token", "issue", "--json", "--name", name, "--ttl", "1h")
		var v struct{ Token string }
		json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &v)
		return v.Token
	}
	alice, bob := issue("alice"), issue("bob")
	for i := 0; i < 3; i++ {
		g.Chat(t, alice, map[string]any{"model": "auto", "messages": msgs("user", fmt.Sprintf("alice 第 %d 次提问，内容长短不一%s", i, strings.Repeat("。", i*20)))}, nil)
	}
	for i := 0; i < 2; i++ {
		g.Chat(t, bob, map[string]any{"model": "local/small", "messages": msgs("user", fmt.Sprintf("bob 问题 %d", i))}, nil)
	}
	cloud.SetDown(true)
	r := g.Chat(t, bob, map[string]any{"model": "auto", "messages": msgs("user", "bob 在外部宕机时提问")}, nil)
	cloud.SetDown(false)
	rep.Check("故障转移的请求也计量", r.Status == 200 && header(r, "X-Gatehouse-Model") == "local/small", "model=%s", header(r, "X-Gatehouse-Model"))
	g.Chat(t, g.Token, map[string]any{"model": "cloud/strong", "messages": msgs("user", "本机令牌")}, nil)
	g.Stop() // SIGTERM flushes the OTLP exporter

	var rows []struct {
		TokenID          string  `json:"token_id"`
		TokenName        string  `json:"token_name"`
		Model            string  `json:"model"`
		Requests         float64 `json:"requests"`
		Errors           float64 `json:"errors"`
		PromptTokens     float64 `json:"prompt_tokens"`
		CompletionTokens float64 `json:"completion_tokens"`
		Cost             float64 `json:"cost"`
	}
	if err := json.Unmarshal([]byte(g.CLI(t, "report", "--format", "json")), &rows); err != nil {
		t.Fatal(err)
	}
	type agg struct{ req, prompt, completion, cost float64 }
	cli := map[string]*agg{}
	names := map[string]string{}
	for _, row := range rows {
		a := cli[row.TokenID]
		if a == nil {
			a = &agg{}
			cli[row.TokenID] = a
		}
		names[row.TokenID] = row.TokenName
		a.req += row.Requests
		a.prompt += row.PromptTokens
		a.completion += row.CompletionTokens
		a.cost += row.Cost
	}
	otReq, otPrompt, otComp, otCost := col.byToken("gatehouse.requests"), col.byToken("gatehouse.tokens.prompt"), col.byToken("gatehouse.tokens.completion"), col.byToken("gatehouse.cost")
	rep.Check("收集器收到导出", col.posts > 0, "收到 %d 次 OTLP 推送", col.posts)
	rep.Check("报表覆盖三把令牌", len(cli) == 3, "令牌：%v", names)
	ids := make([]string, 0, len(cli))
	for id := range cli {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return names[ids[i]] < names[ids[j]] })
	var b strings.Builder
	b.WriteString("## 按令牌核对\n\n| 令牌 | 请求（CLI / OTLP） | prompt | completion | 成本 |\n| --- | --- | --- | --- | --- |\n")
	for _, id := range ids {
		a := cli[id]
		ok := a.req == otReq[id] && a.prompt == otPrompt[id] && a.completion == otComp[id] && math.Abs(a.cost-otCost[id]) < 1e-9
		rep.Check("令牌 "+names[id]+" 汇总一致", ok, "requests %v/%v prompt %v/%v completion %v/%v cost %.6f/%.6f",
			a.req, otReq[id], a.prompt, otPrompt[id], a.completion, otComp[id], a.cost, otCost[id])
		fmt.Fprintf(&b, "| %s | %v / %v | %v / %v | %v / %v | %.6f / %.6f |\n", names[id], a.req, otReq[id], a.prompt, otPrompt[id], a.completion, otComp[id], a.cost, otCost[id])
	}
	rep.Note("%s", b.String())
	rep.Note("CLI 报表（`gatehouse report --format md`）：\n\n%s", g.CLI(t, "report", "--format", "md"))
}
