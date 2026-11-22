package tiered

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

const (
	maxStarterBodyBytes = 512 * 1024 // 512 KiB cap per starter template
	starterLCPMinBytes  = 2048       // minimum shared static prefix bytes to accept LCP shrink
	starterHeadSigBytes = 512        // opening bytes used for family grouping
	starterWarmTimeout  = 120 * time.Second
)

// ErrStarterNotFound indicates an unknown starter hash.
var ErrStarterNotFound = errors.New("starter hash not found")

// StarterCacheConfig mirrors config.TieredStarterCache.
type StarterCacheConfig = config.TieredStarterCache

func defaultStarterConfig(defaultPersist string) StarterCacheConfig {
	return StarterCacheConfig{
		Enabled:           true,
		CanonicalizeTools: true,
		MinTokens:         1024,
		MaxTokens:         100000,
		MinHits:           2,
		MaxEntries:        32,
		KeepAliveSec:      240,
		CheckIntervalSec:  30,
		StartupDelaySec:   15,
		EntryTTLHours:     168,
		PersistPath:       defaultPersist,
	}
}

func applyStarterDefaults(c *StarterCacheConfig, defaultPersist string) {
	def := defaultStarterConfig(defaultPersist)
	if c.MinTokens <= 0 {
		c.MinTokens = def.MinTokens
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = def.MaxTokens
	}
	if c.MinHits <= 0 {
		c.MinHits = def.MinHits
	}
	if c.MaxEntries <= 0 {
		c.MaxEntries = def.MaxEntries
	}
	if c.KeepAliveSec <= 0 {
		c.KeepAliveSec = def.KeepAliveSec
	}
	if c.CheckIntervalSec <= 0 {
		c.CheckIntervalSec = def.CheckIntervalSec
	}
	if c.StartupDelaySec <= 0 {
		c.StartupDelaySec = def.StartupDelaySec
	}
	if c.EntryTTLHours <= 0 {
		c.EntryTTLHours = def.EntryTTLHours
	}
	if c.PersistPath == "" {
		c.PersistPath = def.PersistPath
	}
}

// StarterEntry represents one tracked system+tools starter prefix family.
type StarterEntry struct {
	Hash           string                     `json:"hash"`
	OriginTarget   string                     `json:"origin_target,omitempty"`
	Model          string                     `json:"model"`
	ClientHint     string                     `json:"client_hint,omitempty"`
	Label          string                     `json:"label,omitempty"`
	SystemMsgs     []json.RawMessage          `json:"system_msgs,omitempty"`
	Tools          json.RawMessage            `json:"tools,omitempty"`
	ToolsCount     int                        `json:"tools_count"`
	ExtraOpts      map[string]json.RawMessage `json:"extra_opts,omitempty"`
	EstTokens      int                        `json:"est_tokens"`
	Hits           int64                      `json:"hits"`
	Pinned         bool                       `json:"pinned"`
	LCPShrunk      bool                       `json:"lcp_shrunk"`
	FirstSeen      time.Time                  `json:"first_seen"`
	LastSeen       time.Time                  `json:"last_seen"`
	LastUsed       time.Time                  `json:"last_used"`
	LastWarmed     time.Time                  `json:"last_warmed,omitempty"`
	LastWarmTarget string                     `json:"last_warm_target,omitempty"`
	LastWarmPrompt int                        `json:"last_warm_prompt,omitempty"`
	LastWarmCached int                        `json:"last_warm_cached,omitempty"`
	LastWarmMs     int64                      `json:"last_warm_ms,omitempty"`
	LastWarmErr    string                     `json:"last_warm_err,omitempty"`
	NeedsWarm      bool                       `json:"-"`
}

// StarterView is a redacted view of a StarterEntry for admin APIs (omits full prompt text).
type StarterView struct {
	Hash           string `json:"hash"`
	Label          string `json:"label,omitempty"`
	Model          string `json:"model"`
	ClientHint     string `json:"client_hint,omitempty"`
	ToolsCount     int    `json:"tools_count"`
	EstTokens      int    `json:"est_tokens"`
	Hits           int64  `json:"hits"`
	Pinned         bool   `json:"pinned"`
	Qualified      bool   `json:"qualified"`
	LCPShrunk      bool   `json:"lcp_shrunk"`
	SystemPreview  string `json:"system_preview,omitempty"`
	FirstSeen      string `json:"first_seen"`
	LastSeen       string `json:"last_seen"`
	LastWarmed     string `json:"last_warmed,omitempty"`
	LastWarmTarget string `json:"last_warm_target,omitempty"`
	LastWarmPrompt int    `json:"last_warm_prompt,omitempty"`
	LastWarmCached int    `json:"last_warm_cached,omitempty"`
	LastWarmMs     int64  `json:"last_warm_ms,omitempty"`
	LastWarmErr    string `json:"last_warm_err,omitempty"`
}

type StarterTemplate struct {
	FamilyHash   string
	OriginTarget string
	Model        string
	ClientHint   string
	SystemMsgs   []json.RawMessage
	Tools        json.RawMessage
	ToolsCount   int
	ExtraOpts    map[string]json.RawMessage
	EstTokens    int
}

func normalizeOriginTarget(t string) string {
	if t == "external" {
		return "external"
	}
	return "local"
}

type StarterCache struct {
	router *Router

	mu      sync.RWMutex
	cfg     StarterCacheConfig
	entries map[string]*StarterEntry
	dirty   bool

	warmMu     sync.Mutex
	warmCancel context.CancelFunc

	warming        atomic.Bool
	toolsReordered atomic.Int64
	totalWarms     atomic.Int64
	totalWarmHits  atomic.Int64
	totalWarmFails atomic.Int64
	lastWarmAt     atomic.Value // time.Time
}

func newStarterCache(cfg StarterCacheConfig, router *Router) *StarterCache {
	sc := &StarterCache{
		cfg:     cfg,
		router:  router,
		entries: make(map[string]*StarterEntry),
	}
	sc.loadFromDisk(cfg.PersistPath)
	return sc
}

func (sc *StarterCache) getCfg() StarterCacheConfig {
	sc.mu.RLock()
	c := sc.cfg
	sc.mu.RUnlock()
	return c
}

func (sc *StarterCache) applyConfig(nextCfg StarterCacheConfig) {
	sc.mu.Lock()
	oldPath := sc.cfg.PersistPath
	sc.cfg = nextCfg
	needSwapDisk := oldPath != nextCfg.PersistPath
	sc.mu.Unlock()

	if needSwapDisk {
		if oldPath != "" {
			sc.saveToPath(oldPath)
		}
		if nextCfg.PersistPath != "" {
			sc.loadFromDisk(nextCfg.PersistPath)
		}
	}
}

// canonicalizeTools deterministically sorts the top-level OpenAI "tools" array by tool name.
// If tools is absent, has <=1 entry, or is already sorted, returns body untouched (zero-copy).
func canonicalizeTools(body []byte) ([]byte, bool, int) {
	if !bytes.Contains(body, []byte(`"tools"`)) {
		return body, false, 0
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return body, false, 0
	}
	rawTools, ok := top["tools"]
	if !ok {
		return body, false, 0
	}
	trimmed := bytes.TrimSpace(rawTools)
	if len(trimmed) < 2 || trimmed[0] != '[' {
		return body, false, 0
	}
	var tools []json.RawMessage
	if err := json.Unmarshal(trimmed, &tools); err != nil {
		return body, false, 0
	}
	if len(tools) <= 1 {
		return body, false, len(tools)
	}

	type keyedTool struct {
		key string
		raw json.RawMessage
	}
	items := make([]keyedTool, len(tools))
	for i, raw := range tools {
		items[i] = keyedTool{
			key: extractToolSortKey(raw),
			raw: raw,
		}
	}
	if sort.SliceIsSorted(items, func(i, j int) bool { return items[i].key < items[j].key }) {
		return body, false, len(tools)
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].key < items[j].key })

	sortedTools := make([]json.RawMessage, len(items))
	for i, it := range items {
		sortedTools[i] = it.raw
	}
	newToolsRaw, err := json.Marshal(sortedTools)
	if err != nil {
		return body, false, len(tools)
	}
	top["tools"] = newToolsRaw
	out, err := json.Marshal(top)
	if err != nil {
		return body, false, len(tools)
	}
	return out, true, len(tools)
}

func extractToolSortKey(raw json.RawMessage) string {
	var th struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		Namespace json.RawMessage `json:"namespace"`
		Function  struct {
			Name      string          `json:"name"`
			Namespace json.RawMessage `json:"namespace"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &th); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	name := th.Function.Name
	if name == "" {
		name = th.Name
	}
	ns := extractNamespaceName(th.Function.Namespace)
	if ns == "" {
		ns = extractNamespaceName(th.Namespace)
	}
	return th.Type + "|" + ns + "|" + name
}

func extractNamespaceName(raw json.RawMessage) string {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		return ""
	}
	if b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	if b[0] == '{' {
		var obj struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(b, &obj) == nil {
			return obj.Name
		}
	}
	return ""
}

// prefixOptionKeys are request parameters that alter DeepSeek-V4.1's rendered prefix before the first user turn.
var prefixOptionKeys = []string{
	"reasoning_effort",
	"chat_template_kwargs",
	"enable_thinking",
	"thinking",
	"response_format",
}

func compactJSONBytes(raw []byte) []byte {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) == nil {
		return buf.Bytes()
	}
	return bytes.TrimSpace(raw)
}

func extractStarterTemplate(body []byte, userAgent string, minTokens, maxTokens int) *StarterTemplate {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}
	rawMsgs, ok := top["messages"]
	if !ok {
		return nil
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil || len(msgs) == 0 {
		return nil
	}

	var sysMsgs []json.RawMessage
	sysBytes := 0
	sysEst := 0
	for _, m := range msgs {
		var hdr struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(m, &hdr); err != nil {
			break
		}
		if hdr.Role != "system" && hdr.Role != "developer" {
			break
		}
		if bytes.Contains(hdr.Content, []byte(`"image_url"`)) || bytes.Contains(hdr.Content, []byte(`"input_image"`)) {
			return nil
		}
		trimmed := bytes.TrimSpace(m)
		sysMsgs = append(sysMsgs, append(json.RawMessage(nil), trimmed...))
		sysBytes += len(trimmed)
		sysEst += estimateMessageTokens(trimmed)
	}

	var toolsRaw json.RawMessage
	toolsCount := 0
	toolsEst := 0
	if rawT, ok := top["tools"]; ok {
		t := bytes.TrimSpace(rawT)
		if len(t) >= 2 && t[0] == '[' && !bytes.Equal(t, []byte("[]")) {
			var arr []json.RawMessage
			if json.Unmarshal(t, &arr) == nil && len(arr) > 0 {
				toolsRaw = append(json.RawMessage(nil), t...)
				toolsCount = len(arr)
				toolsEst = estimateTextTokens(string(t))
			}
		}
	}

	if len(sysMsgs) == 0 && toolsCount == 0 {
		return nil
	}
	if sysBytes+len(toolsRaw) > maxStarterBodyBytes {
		return nil
	}
	totalEst := sysEst + toolsEst
	if totalEst < minTokens || totalEst > maxTokens {
		return nil
	}

	model := "DeepSeek-V4.1-Flash-1017"
	if rawM, ok := top["model"]; ok {
		var m string
		if json.Unmarshal(rawM, &m) == nil && m != "" {
			model = m
		}
	}

	extraOpts := make(map[string]json.RawMessage)
	for _, k := range prefixOptionKeys {
		if v, ok := top[k]; ok {
			tv := bytes.TrimSpace(v)
			if len(tv) > 0 && !bytes.Equal(tv, []byte("null")) {
				extraOpts[k] = append(json.RawMessage(nil), tv...)
			}
		}
	}

	h := sha256.New()
	h.Write([]byte("v1|"))
	h.Write([]byte(model))
	h.Write([]byte("|"))
	for _, k := range prefixOptionKeys {
		h.Write([]byte(k))
		h.Write([]byte("="))
		if v, ok := extraOpts[k]; ok {
			h.Write(compactJSONBytes(v))
		}
		h.Write([]byte(";"))
	}
	h.Write([]byte("|tools="))
	if len(toolsRaw) > 0 {
		th := sha256.Sum256(compactJSONBytes(toolsRaw))
		h.Write(th[:])
	}
	h.Write([]byte("|head="))
	if len(sysMsgs) > 0 {
		headText := extractMessagePlainText(sysMsgs[0])
		if len(headText) > starterHeadSigBytes {
			headText = truncateUTF8(headText, starterHeadSigBytes)
		}
		h.Write([]byte(headText))
	} else {
		h.Write([]byte("<no-sys>"))
	}
	familyHash := hex.EncodeToString(h.Sum(nil)[:16])

	return &StarterTemplate{
		FamilyHash: familyHash,
		Model:      model,
		ClientHint: sanitizeClientHint(userAgent),
		SystemMsgs: sysMsgs,
		Tools:      toolsRaw,
		ToolsCount: toolsCount,
		ExtraOpts:  extraOpts,
		EstTokens:  totalEst,
	}
}

func sanitizeClientHint(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) > 80 {
		ua = ua[:80]
	}
	return ua
}

func extractMessagePlainText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &msg); err != nil {
		return ""
	}
	c := bytes.TrimSpace(msg.Content)
	if len(c) == 0 {
		return ""
	}
	if c[0] == '"' {
		var s string
		if json.Unmarshal(c, &s) == nil {
			return s
		}
	}
	if c[0] == '[' {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c, &parts) == nil {
			var sb strings.Builder
			for i, p := range parts {
				if p.Text != "" {
					if i > 0 {
						sb.WriteString("\n")
					}
					sb.WriteString(p.Text)
				}
			}
			return sb.String()
		}
	}
	return ""
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// commonPrefixClean returns the longest common UTF-8 prefix of a and b, snapped back to a clean newline/space boundary.
func commonPrefixClean(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	if i == len(a) && i == len(b) {
		return a
	}
	for i > 0 && i < len(a) && !utf8.RuneStart(a[i]) {
		i--
	}
	prefix := a[:i]
	if nl := strings.LastIndexByte(prefix, '\n'); nl > 0 && len(prefix)-nl <= 256 {
		return strings.TrimRightFunc(prefix[:nl], unicode.IsSpace)
	}
	for j := len(prefix) - 1; j >= 0 && len(prefix)-j <= 64; j-- {
		if prefix[j] == ' ' || prefix[j] == '\t' || prefix[j] == '\n' {
			return strings.TrimRightFunc(prefix[:j], unicode.IsSpace)
		}
	}
	return prefix
}

func systemMsgsEqual(a, b []json.RawMessage) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal(bytes.TrimSpace(a[i]), bytes.TrimSpace(b[i])) {
			return false
		}
	}
	return true
}

func shrinkSystemMessages(existing, incoming []json.RawMessage) ([]json.RawMessage, bool, bool) {
	if systemMsgsEqual(existing, incoming) {
		return existing, false, true
	}
	minLen := len(existing)
	if len(incoming) < minLen {
		minLen = len(incoming)
	}
	var out []json.RawMessage
	for i := 0; i < minLen; i++ {
		eb := bytes.TrimSpace(existing[i])
		ib := bytes.TrimSpace(incoming[i])
		if bytes.Equal(eb, ib) {
			out = append(out, eb)
			continue
		}
		shrunkMsg, ok := shrinkSingleSystemMessage(eb, ib)
		if ok {
			out = append(out, shrunkMsg)
		}
		break
	}
	if len(out) == 0 {
		return nil, false, false
	}
	return out, true, true
}

func shrinkSingleSystemMessage(aRaw, bRaw json.RawMessage) (json.RawMessage, bool) {
	var aMsg, bMsg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(aRaw, &aMsg) != nil || json.Unmarshal(bRaw, &bMsg) != nil {
		return nil, false
	}
	if aMsg.Role != bMsg.Role {
		return nil, false
	}
	ac := bytes.TrimSpace(aMsg.Content)
	bc := bytes.TrimSpace(bMsg.Content)

	if len(ac) > 0 && ac[0] == '"' && len(bc) > 0 && bc[0] == '"' {
		var as, bs string
		if json.Unmarshal(ac, &as) != nil || json.Unmarshal(bc, &bs) != nil {
			return nil, false
		}
		lcp := commonPrefixClean(as, bs)
		if len(lcp) < starterLCPMinBytes {
			return nil, false
		}
		b, err := json.Marshal(map[string]any{
			"role":    aMsg.Role,
			"content": lcp,
		})
		if err != nil {
			return nil, false
		}
		return b, true
	}

	if len(ac) > 0 && ac[0] == '[' && len(bc) > 0 && bc[0] == '[' {
		var aParts, bParts []map[string]any
		if json.Unmarshal(ac, &aParts) != nil || json.Unmarshal(bc, &bParts) != nil {
			return nil, false
		}
		minP := len(aParts)
		if len(bParts) < minP {
			minP = len(bParts)
		}
		var kept []map[string]any
		totalBytes := 0
		for i := 0; i < minP; i++ {
			at, _ := aParts[i]["type"].(string)
			bt, _ := bParts[i]["type"].(string)
			if at != "text" || bt != "text" {
				break
			}
			as, _ := aParts[i]["text"].(string)
			bs, _ := bParts[i]["text"].(string)
			if as == bs {
				kept = append(kept, map[string]any{"type": "text", "text": as})
				totalBytes += len(as)
				continue
			}
			lcp := commonPrefixClean(as, bs)
			if len(lcp) >= starterLCPMinBytes || (totalBytes >= starterLCPMinBytes && len(lcp) >= 256) {
				kept = append(kept, map[string]any{"type": "text", "text": lcp})
				totalBytes += len(lcp)
			}
			break
		}
		if totalBytes < starterLCPMinBytes || len(kept) == 0 {
			return nil, false
		}
		b, err := json.Marshal(map[string]any{
			"role":    aMsg.Role,
			"content": kept,
		})
		if err != nil {
			return nil, false
		}
		return b, true
	}

	return nil, false
}

func estimateStarterTokens(sysMsgs []json.RawMessage, tools json.RawMessage) int {
	total := 0
	for _, m := range sysMsgs {
		total += estimateMessageTokens(m)
	}
	if len(tools) > 0 {
		total += estimateTextTokens(string(tools))
	}
	return total
}

func (sc *StarterCache) Observe(tmpl *StarterTemplate) {
	if tmpl == nil {
		return
	}
	cfg := sc.getCfg()
	if !cfg.Enabled {
		return
	}
	origin := normalizeOriginTarget(tmpl.OriginTarget)
	if sc.router != nil && sc.router.getTarget() != "local" && origin != "external" {
		return
	}
	now := time.Now()
	sc.mu.Lock()
	defer sc.mu.Unlock()

	if existing, ok := sc.entries[tmpl.FamilyHash]; ok {
		if normalizeOriginTarget(existing.OriginTarget) != origin {
			sc.entries[tmpl.FamilyHash] = &StarterEntry{
				Hash:         tmpl.FamilyHash,
				OriginTarget: origin,
				Model:        tmpl.Model,
				ClientHint:   tmpl.ClientHint,
				SystemMsgs:   tmpl.SystemMsgs,
				Tools:        tmpl.Tools,
				ToolsCount:   tmpl.ToolsCount,
				ExtraOpts:    tmpl.ExtraOpts,
				EstTokens:    tmpl.EstTokens,
				Hits:         1,
				FirstSeen:    now,
				LastSeen:     now,
				LastUsed:     now,
			}
			sc.dirty = true
			return
		}
		existing.Hits++
		existing.LastSeen = now
		existing.LastUsed = now
		if existing.ClientHint == "" && tmpl.ClientHint != "" {
			existing.ClientHint = tmpl.ClientHint
		}
		if !existing.Pinned && !systemMsgsEqual(existing.SystemMsgs, tmpl.SystemMsgs) {
			if shrunk, didShrink, ok := shrinkSystemMessages(existing.SystemMsgs, tmpl.SystemMsgs); ok && didShrink {
				newEst := estimateStarterTokens(shrunk, existing.Tools)
				if newEst >= cfg.MinTokens {
					existing.SystemMsgs = shrunk
					existing.EstTokens = newEst
					existing.LCPShrunk = true
					existing.LastWarmed = time.Time{}
					existing.NeedsWarm = true
					sc.dirty = true
					sc.router.log.Info("starter LCP-shrunk", "provider", sc.router.name, "family", existing.Hash[:12], "client", existing.ClientHint, "est_tokens", existing.EstTokens, "hits", existing.Hits)
				}
			}
		}
		if existing.Hits == int64(cfg.MinHits) {
			sc.dirty = true
			sc.router.log.Info("starter auto-qualified", "provider", sc.router.name, "family", existing.Hash[:12], "client", existing.ClientHint, "est_tokens", existing.EstTokens, "tools", existing.ToolsCount, "hits", existing.Hits)
		}
		return
	}

	sc.entries[tmpl.FamilyHash] = &StarterEntry{
		Hash:         tmpl.FamilyHash,
		OriginTarget: origin,
		Model:        tmpl.Model,
		ClientHint:   tmpl.ClientHint,
		SystemMsgs:   tmpl.SystemMsgs,
		Tools:        tmpl.Tools,
		ToolsCount:   tmpl.ToolsCount,
		ExtraOpts:    tmpl.ExtraOpts,
		EstTokens:    tmpl.EstTokens,
		Hits:         1,
		FirstSeen:    now,
		LastSeen:     now,
		LastUsed:     now,
	}
	sc.trimLocked()
	sc.dirty = true
}

func (sc *StarterCache) enforceTarget(routerTarget string) {
	if routerTarget == "local" {
		return
	}
	sc.mu.Lock()
	removed := 0
	for k, e := range sc.entries {
		if normalizeOriginTarget(e.OriginTarget) != "external" {
			delete(sc.entries, k)
			removed++
		}
	}
	if removed > 0 {
		sc.dirty = true
	}
	sc.mu.Unlock()
	if removed > 0 {
		sc.router.log.Info("purged non-external starter prefixes after target change", "provider", sc.router.name, "removed", removed)
		sc.saveToDisk()
	}
}

func (sc *StarterCache) trimLocked() {
	ttl := time.Duration(sc.cfg.EntryTTLHours) * time.Hour
	now := time.Now()
	for k, e := range sc.entries {
		if !e.Pinned && now.Sub(e.LastSeen) > ttl {
			delete(sc.entries, k)
			sc.dirty = true
		}
	}
	if len(sc.entries) <= sc.cfg.MaxEntries {
		return
	}
	all := make([]*StarterEntry, 0, len(sc.entries))
	for _, e := range sc.entries {
		all = append(all, e)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Pinned != all[j].Pinned {
			return all[i].Pinned
		}
		if all[i].Hits != all[j].Hits {
			return all[i].Hits > all[j].Hits
		}
		return all[i].LastSeen.After(all[j].LastSeen)
	})
	for _, e := range all[sc.cfg.MaxEntries:] {
		if !e.Pinned {
			delete(sc.entries, e.Hash)
			sc.dirty = true
		}
	}
}

func (sc *StarterCache) loadFromDisk(path string) {
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			sc.router.log.Warn("starter load failed", "provider", sc.router.name, "path", path, "err", err)
		}
		return
	}
	var list []*StarterEntry
	if err := json.Unmarshal(data, &list); err != nil {
		sc.router.log.Warn("starter parse failed", "provider", sc.router.name, "path", path, "err", err)
		return
	}
	routerTarget := sc.router.getTarget()
	sc.mu.Lock()
	dropped := 0
	for _, e := range list {
		if e != nil && e.Hash != "" {
			if routerTarget != "local" && normalizeOriginTarget(e.OriginTarget) != "external" {
				dropped++
				continue
			}
			e.LastWarmed = time.Time{}
			e.NeedsWarm = true
			sc.entries[e.Hash] = e
		}
	}
	if dropped > 0 {
		sc.dirty = true
	}
	count := len(sc.entries)
	sc.mu.Unlock()
	if dropped > 0 {
		sc.saveToPath(path)
	}
	sc.router.log.Info("loaded starter prefixes", "provider", sc.router.name, "count", count, "path", path)
}

func (sc *StarterCache) saveToDisk() {
	sc.saveToPath(sc.getCfg().PersistPath)
}

func (sc *StarterCache) saveToPath(path string) {
	if path == "" {
		return
	}
	sc.mu.Lock()
	if !sc.dirty {
		sc.mu.Unlock()
		return
	}
	sc.trimLocked()
	minHits := sc.cfg.MinHits
	list := make([]*StarterEntry, 0, len(sc.entries))
	for _, e := range sc.entries {
		if e.Pinned || e.Hits >= int64(minHits) {
			cp := *e
			list = append(list, &cp)
		}
	}
	sc.dirty = false
	sc.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		sc.router.log.Warn("starter mkdir failed", "provider", sc.router.name, "path", path, "err", err)
		return
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		sc.router.log.Warn("starter marshal failed", "provider", sc.router.name, "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		sc.router.log.Warn("starter temp write failed", "provider", sc.router.name, "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		sc.router.log.Warn("starter rename failed", "provider", sc.router.name, "path", path, "err", err)
	}
}

func (sc *StarterCache) syncWarmLoop(parent context.Context, shouldRun bool) {
	sc.warmMu.Lock()
	if !shouldRun {
		if sc.warmCancel != nil {
			sc.warmCancel()
			sc.warmCancel = nil
		}
		sc.warmMu.Unlock()
		return
	}
	if sc.warmCancel != nil {
		sc.warmMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	sc.warmCancel = cancel
	sc.warmMu.Unlock()

	go func() {
		cfg := sc.getCfg()
		timer := time.NewTimer(time.Duration(cfg.StartupDelaySec) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		sc.WarmDue(false, "")
		for {
			cur := sc.getCfg()
			interval := cur.CheckIntervalSec
			if interval <= 0 {
				interval = 30
			}
			tk := time.NewTimer(time.Duration(interval) * time.Second)
			select {
			case <-ctx.Done():
				tk.Stop()
				return
			case <-tk.C:
				sc.WarmDue(false, "")
				sc.saveToDisk()
			}
		}
	}()
}

func (sc *StarterCache) buildWarmupPayload(e *StarterEntry) ([]byte, error) {
	msgs := make([]json.RawMessage, 0, len(e.SystemMsgs)+1)
	msgs = append(msgs, e.SystemMsgs...)
	msgs = append(msgs, json.RawMessage(`{"role":"user","content":"."}`))

	model := e.Model
	if model == "" {
		model = "DeepSeek-V4.1-Flash-1017"
	}
	payload := map[string]any{
		"model":       model,
		"messages":    msgs,
		"max_tokens":  1,
		"temperature": 0,
		"stream":      false,
	}
	if len(e.Tools) > 0 {
		payload["tools"] = e.Tools
	}
	for k, v := range e.ExtraOpts {
		payload[k] = v
	}
	return json.Marshal(payload)
}

func (sc *StarterCache) selectIdleBackendForWarm(estTokens int) *BackendState {
	for _, b := range sc.router.getBackends() {
		if !b.healthy.Load() {
			continue
		}
		if b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
			continue
		}
		bCfg := b.config()
		if estTokens+64 <= bCfg.MaxSafeInput {
			return b
		}
	}
	return nil
}

type WarmResult struct {
	Hash         string `json:"hash"`
	Backend      string `json:"backend,omitempty"`
	PromptTokens int    `json:"prompt_tokens,omitempty"`
	CachedTokens int    `json:"cached_tokens,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	Error        string `json:"error,omitempty"`
}

func (sc *StarterCache) WarmDue(force bool, onlyHash string) []WarmResult {
	cfg := sc.getCfg()
	if !cfg.Enabled {
		return nil
	}
	if !sc.warming.CompareAndSwap(false, true) {
		return nil
	}
	defer sc.warming.Store(false)

	now := time.Now()
	keepAlive := time.Duration(cfg.KeepAliveSec) * time.Second

	sc.mu.RLock()
	var due []*StarterEntry
	for _, e := range sc.entries {
		if onlyHash != "" && e.Hash != onlyHash && !strings.HasPrefix(e.Hash, onlyHash) {
			continue
		}
		if !force && !e.Pinned && e.Hits < int64(cfg.MinHits) {
			continue
		}
		lastTouch := e.LastWarmed
		if e.LastUsed.After(lastTouch) {
			lastTouch = e.LastUsed
		}
		if !force && !e.NeedsWarm && !lastTouch.IsZero() && now.Sub(lastTouch) < keepAlive {
			continue
		}
		cp := *e
		due = append(due, &cp)
	}
	sc.mu.RUnlock()

	if len(due) == 0 {
		return nil
	}
	sort.Slice(due, func(i, j int) bool {
		if due[i].Pinned != due[j].Pinned {
			return due[i].Pinned
		}
		return due[i].Hits > due[j].Hits
	})

	var results []WarmResult
	for idx, entry := range due {
		if idx > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		backend := sc.selectIdleBackendForWarm(entry.EstTokens)
		if backend == nil {
			if force {
				results = append(results, WarmResult{Hash: entry.Hash, Error: "no idle backend available"})
			}
			break
		}
		res := sc.warmOne(entry, backend)
		results = append(results, res)
	}
	return results
}

func (sc *StarterCache) warmOne(entry *StarterEntry, backend *BackendState) WarmResult {
	start := time.Now()
	bCfg, bTarget := backend.snapshot()
	routerTarget := sc.router.getTarget()
	if routerTarget != "local" && normalizeOriginTarget(entry.OriginTarget) != "external" {
		sc.mu.Lock()
		delete(sc.entries, entry.Hash)
		sc.dirty = true
		sc.mu.Unlock()
		sc.saveToDisk()
		sc.totalWarmFails.Add(1)
		sc.router.log.Warn("starter warmup skipped: trust boundary mismatch", "provider", sc.router.name, "hash", entry.Hash[:min(12, len(entry.Hash))], "origin", entry.OriginTarget, "target", routerTarget)
		return WarmResult{Hash: entry.Hash, Backend: bCfg.ID, Error: "starter trust boundary mismatch"}
	}
	payload, err := sc.buildWarmupPayload(entry)
	if err != nil {
		sc.totalWarmFails.Add(1)
		return WarmResult{Hash: entry.Hash, Error: err.Error()}
	}

	ctx, cancel := context.WithTimeout(context.Background(), starterWarmTimeout)
	defer cancel()

	if fn := sc.router.getWarmupFilter(); fn != nil {
		filtered, ferr := fn(ctx, sc.router.name, bCfg.URL, payload)
		if ferr != nil {
			errMsg := ferr.Error()
			sc.totalWarmFails.Add(1)
			sc.mu.Lock()
			if cur, ok := sc.entries[entry.Hash]; ok {
				cur.LastWarmErr = errMsg
			}
			sc.mu.Unlock()
			sc.router.log.Warn("starter warmup blocked by redaction policy", "provider", sc.router.name, "hash", entry.Hash[:min(12, len(entry.Hash))], "backend", bCfg.ID, "err", errMsg)
			return WarmResult{Hash: entry.Hash, Backend: bCfg.ID, Error: errMsg}
		}
		payload = filtered
	}

	targetURL := backendChatCompletionsURL(bTarget, bCfg.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(payload))
	if err != nil {
		sc.totalWarmFails.Add(1)
		return WarmResult{Hash: entry.Hash, Backend: bCfg.ID, Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dsv41-smart-router-warmer/1.0")
	if apiKey := sc.router.getAPIKey(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	backend.activeReqs.Add(1)
	resp, err := sc.router.transport.RoundTrip(req)
	var respBody []byte
	var statusCode int
	if err == nil {
		statusCode = resp.StatusCode
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		_ = resp.Body.Close()
	}
	backend.activeReqs.Add(-1)

	durMs := time.Since(start).Milliseconds()
	res := WarmResult{
		Hash:       entry.Hash,
		Backend:    bCfg.ID,
		DurationMs: durMs,
	}

	if err != nil || statusCode < 200 || statusCode >= 300 {
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else {
			errMsg = fmt.Sprintf("HTTP %d: %s", statusCode, truncateUTF8(string(respBody), 160))
		}
		res.Error = errMsg
		sc.totalWarmFails.Add(1)
		sc.mu.Lock()
		if cur, ok := sc.entries[entry.Hash]; ok {
			cur.LastWarmErr = errMsg
			cur.LastWarmMs = durMs
		}
		sc.mu.Unlock()
		sc.router.log.Warn("starter warm failed", "provider", sc.router.name, "hash", entry.Hash[:min(12, len(entry.Hash))], "backend", bCfg.ID, "ms", durMs, "err", errMsg)
		return res
	}

	promptToks, cachedToks := extractUsageFromTail(respBody)
	res.PromptTokens = promptToks
	res.CachedTokens = cachedToks
	now := time.Now()
	sc.totalWarms.Add(1)
	if promptToks > 0 && cachedToks*2 >= promptToks {
		sc.totalWarmHits.Add(1)
	}
	sc.lastWarmAt.Store(now)

	sc.mu.Lock()
	if cur, ok := sc.entries[entry.Hash]; ok {
		cur.LastWarmed = now
		cur.NeedsWarm = false
		cur.LastWarmTarget = bCfg.ID
		cur.LastWarmPrompt = promptToks
		cur.LastWarmCached = cachedToks
		cur.LastWarmMs = durMs
		cur.LastWarmErr = ""
		if promptToks > 16 {
			cur.EstTokens = promptToks - 10
		}
		sc.dirty = true
	}
	sc.mu.Unlock()

	if !entry.LCPShrunk && len(entry.SystemMsgs) > 0 {
		prefixTokens := entry.EstTokens
		if promptToks > 16 {
			prefixTokens = promptToks - 10
		}
		sc.router.seedWarmedPrefix(entry.Tools, entry.SystemMsgs, prefixTokens, bCfg.ID)
	}

	sc.router.log.Info("starter warmed", "provider", sc.router.name, "hash", entry.Hash[:min(12, len(entry.Hash))], "backend", bCfg.ID, "client", entry.ClientHint, "prompt", promptToks, "cached", cachedToks, "ms", durMs)
	return res
}

func (sc *StarterCache) Summary() map[string]any {
	cfg := sc.getCfg()
	sc.mu.RLock()
	tracked := len(sc.entries)
	qualified := 0
	pinned := 0
	for _, e := range sc.entries {
		if e.Pinned {
			pinned++
			qualified++
		} else if e.Hits >= int64(cfg.MinHits) {
			qualified++
		}
	}
	sc.mu.RUnlock()

	lastWarm := ""
	if v, ok := sc.lastWarmAt.Load().(time.Time); ok && !v.IsZero() {
		lastWarm = v.Format(time.RFC3339)
	}
	return map[string]any{
		"enabled":            cfg.Enabled,
		"canonicalize_tools": cfg.CanonicalizeTools,
		"tools_reordered":    sc.toolsReordered.Load(),
		"tracked":            tracked,
		"active_warm_set":    qualified,
		"pinned":             pinned,
		"warming_now":        sc.warming.Load(),
		"keepalive_sec":      cfg.KeepAliveSec,
		"min_hits":           cfg.MinHits,
		"min_tokens":         cfg.MinTokens,
		"total_warms":        sc.totalWarms.Load(),
		"total_warm_hits":    sc.totalWarmHits.Load(),
		"total_warm_fails":   sc.totalWarmFails.Load(),
		"last_warm_at":       lastWarm,
	}
}

// List returns sanitized starter entries (with short 120-char preview instead of full prompt text).
func (sc *StarterCache) List() []StarterView {
	cfg := sc.getCfg()
	sc.mu.RLock()
	items := make([]StarterView, 0, len(sc.entries))
	for _, e := range sc.entries {
		preview := ""
		if len(e.SystemMsgs) > 0 {
			preview = strings.ReplaceAll(extractMessagePlainText(e.SystemMsgs[0]), "\n", " ")
			if len(preview) > 120 {
				preview = truncateUTF8(preview, 120) + "..."
			}
		}
		lw := ""
		if !e.LastWarmed.IsZero() {
			lw = e.LastWarmed.Format(time.RFC3339)
		}
		items = append(items, StarterView{
			Hash:           e.Hash,
			Label:          e.Label,
			Model:          e.Model,
			ClientHint:     e.ClientHint,
			ToolsCount:     e.ToolsCount,
			EstTokens:      e.EstTokens,
			Hits:           e.Hits,
			Pinned:         e.Pinned,
			Qualified:      e.Pinned || e.Hits >= int64(cfg.MinHits),
			LCPShrunk:      e.LCPShrunk,
			SystemPreview:  preview,
			FirstSeen:      e.FirstSeen.Format(time.RFC3339),
			LastSeen:       e.LastSeen.Format(time.RFC3339),
			LastWarmed:     lw,
			LastWarmTarget: e.LastWarmTarget,
			LastWarmPrompt: e.LastWarmPrompt,
			LastWarmCached: e.LastWarmCached,
			LastWarmMs:     e.LastWarmMs,
			LastWarmErr:    e.LastWarmErr,
		})
	}
	sc.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		if items[i].Pinned != items[j].Pinned {
			return items[i].Pinned
		}
		return items[i].Hits > items[j].Hits
	})
	return items
}

// Pin updates pinned status (and optional label) on a tracked starter matched by exact or prefix hash.
func (sc *StarterCache) Pin(hash string, pinned bool, label string) (StarterView, error) {
	if strings.TrimSpace(hash) == "" {
		return StarterView{}, ErrStarterNotFound
	}
	cfg := sc.getCfg()
	sc.mu.Lock()
	var matched *StarterEntry
	for k, e := range sc.entries {
		if k == hash || strings.HasPrefix(k, hash) {
			e.Pinned = pinned
			if pinned && e.LastWarmed.IsZero() {
				e.NeedsWarm = true
			}
			if label != "" {
				e.Label = label
			}
			sc.dirty = true
			matched = e
			break
		}
	}
	var out StarterView
	if matched != nil {
		out = StarterView{
			Hash:      matched.Hash,
			Label:     matched.Label,
			Model:     matched.Model,
			Hits:      matched.Hits,
			Pinned:    matched.Pinned,
			Qualified: matched.Pinned || matched.Hits >= int64(cfg.MinHits),
			EstTokens: matched.EstTokens,
		}
	}
	sc.mu.Unlock()
	if matched == nil {
		return StarterView{}, ErrStarterNotFound
	}
	sc.saveToDisk()
	return out, nil
}

// Delete removes a tracked starter matched by exact or prefix hash.
func (sc *StarterCache) Delete(hash string) (string, error) {
	if strings.TrimSpace(hash) == "" {
		return "", ErrStarterNotFound
	}
	sc.mu.Lock()
	deleted := ""
	for k := range sc.entries {
		if k == hash || strings.HasPrefix(k, hash) {
			delete(sc.entries, k)
			deleted = k
			sc.dirty = true
			break
		}
	}
	sc.mu.Unlock()
	if deleted == "" {
		return "", ErrStarterNotFound
	}
	sc.saveToDisk()
	return deleted, nil
}

// Warm forces immediate warm-up of one or all eligible starters.
func (sc *StarterCache) Warm(hash string, force bool) []WarmResult {
	results := sc.WarmDue(force, hash)
	sc.saveToDisk()
	return results
}
