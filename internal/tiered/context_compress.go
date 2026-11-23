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
	"unicode/utf8"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
)

const (
	compressHeaderBypass  = "X-No-Compress"
	compressHeaderControl = "X-Context-Compress"
	summaryHeaderMarker   = "[历史上下文压缩摘要"
	stage2AckContent      = "已收到并牢记以上历史上下文摘要、硬约束与当前状态，我将在此基础上继续协助你。"
	stage2RetryCooldown   = 5 * time.Minute
	maxSummaryRunes       = 16000
)

// ErrCheckpointNotFound indicates an unknown checkpoint id.
var ErrCheckpointNotFound = errors.New("checkpoint not found")

// ContextCompressConfig mirrors config.TieredContextCompress.
type ContextCompressConfig = config.TieredContextCompress

func defaultContextCompressConfig(defaultPersist string) ContextCompressConfig {
	return ContextCompressConfig{
		Enabled:                   false,
		TriggerTokens:             500000,
		Stage1TargetTokens:        180000,
		KeepRecentGroups:          6,
		KeepRecentMinTokens:       24000,
		KeepRecentMaxTokens:       80000,
		ToolMaxChars:              3000,
		MutationToolMaxChars:      4500,
		StaleReadMaxChars:         400,
		UserPasteMaxChars:         20000,
		AssistantMaxChars:         12000,
		Stage2MaxSummaryTokens:    4096,
		Stage2TimeoutSec:          300,
		Stage2WarmStage1MaxTokens: 250000,
		Stage2WarmStage1MinDrop:   25,
		CheckpointTTLHours:        168,
		MaxCheckpoints:            256,
		PersistPath:               defaultPersist,
	}
}

func applyContextCompressDefaults(c *ContextCompressConfig, defaultPersist string) {
	def := defaultContextCompressConfig(defaultPersist)
	if c.TriggerTokens <= 0 {
		c.TriggerTokens = def.TriggerTokens
	}
	if c.Stage1TargetTokens <= 0 {
		c.Stage1TargetTokens = def.Stage1TargetTokens
	}
	if c.KeepRecentGroups <= 0 {
		c.KeepRecentGroups = def.KeepRecentGroups
	}
	if c.KeepRecentMinTokens <= 0 {
		c.KeepRecentMinTokens = def.KeepRecentMinTokens
	}
	if c.KeepRecentMaxTokens <= 0 {
		c.KeepRecentMaxTokens = def.KeepRecentMaxTokens
	}
	if c.ToolMaxChars <= 0 {
		c.ToolMaxChars = def.ToolMaxChars
	}
	if c.MutationToolMaxChars <= 0 {
		c.MutationToolMaxChars = def.MutationToolMaxChars
	}
	if c.StaleReadMaxChars <= 0 {
		c.StaleReadMaxChars = def.StaleReadMaxChars
	}
	if c.UserPasteMaxChars <= 0 {
		c.UserPasteMaxChars = def.UserPasteMaxChars
	}
	if c.AssistantMaxChars <= 0 {
		c.AssistantMaxChars = def.AssistantMaxChars
	}
	if c.Stage2MaxSummaryTokens <= 0 {
		c.Stage2MaxSummaryTokens = def.Stage2MaxSummaryTokens
	}
	if c.Stage2TimeoutSec <= 0 {
		c.Stage2TimeoutSec = def.Stage2TimeoutSec
	}
	if c.Stage2WarmStage1MaxTokens <= 0 {
		c.Stage2WarmStage1MaxTokens = def.Stage2WarmStage1MaxTokens
	}
	if c.Stage2WarmStage1MinDrop <= 0 {
		c.Stage2WarmStage1MinDrop = def.Stage2WarmStage1MinDrop
	}
	if c.CheckpointTTLHours <= 0 {
		c.CheckpointTTLHours = def.CheckpointTTLHours
	}
	if c.MaxCheckpoints <= 0 {
		c.MaxCheckpoints = def.MaxCheckpoints
	}
	if c.PersistPath == "" {
		c.PersistPath = def.PersistPath
	}
}

func stage2Enabled(c ContextCompressConfig) bool {
	if c.Stage2Enabled == nil {
		return true
	}
	return *c.Stage2Enabled
}

func stage2PrewarmEnabled(c ContextCompressConfig) bool {
	if c.Stage2Prewarm == nil {
		return true
	}
	return *c.Stage2Prewarm
}

// ContextCheckpoint holds a frozen prefix compression state keyed by bodyHash.
type ContextCheckpoint struct {
	ID               string            `json:"id"`
	SessionRoot      string            `json:"session_root"`
	OriginTarget     string            `json:"origin_target,omitempty"`
	ScopeHash        string            `json:"scope_hash,omitempty"`
	Confidential     bool              `json:"-"` // never persisted to disk
	CutMsgIdx        int               `json:"cut_msg_idx"`
	ArchivedMsgCount int               `json:"archived_msg_count"`
	NextMsgRole      string            `json:"next_msg_role"`
	Generation       int               `json:"generation"`
	Stage            int               `json:"stage"` // 0=warm-passthrough pending stage2, 1=stage1 pruned, 2=stage2 llm summary
	OriginalTokens   int               `json:"original_tokens"`
	Stage1Tokens     int               `json:"stage1_tokens"`
	SummaryTokens    int               `json:"summary_tokens,omitempty"`
	Stage1Msgs       []json.RawMessage `json:"stage1_msgs,omitempty"`
	SummaryText      string            `json:"summary_text,omitempty"`
	HardConstraints  string            `json:"hard_constraints,omitempty"`
	Stage2Status     string            `json:"stage2_status"` // none|pending|running|ready|failed
	Stage2Backend    string            `json:"stage2_backend,omitempty"`
	Stage2Ms         int64             `json:"stage2_ms,omitempty"`
	Stage2Err        string            `json:"stage2_err,omitempty"`
	Stage2FailedAt   time.Time         `json:"stage2_failed_at,omitempty"`
	PrewarmPrompt    int               `json:"prewarm_prompt,omitempty"`
	PrewarmCached    int               `json:"prewarm_cached,omitempty"`
	Hits             int64             `json:"hits"`
	CreatedAt        time.Time         `json:"created_at"`
	LastUsed         time.Time         `json:"last_used"`
}

// CheckpointView is the sanitized admin API representation of a ContextCheckpoint.
type CheckpointView struct {
	ID               string `json:"id"`
	SessionRoot      string `json:"session_root"`
	OriginTarget     string `json:"origin_target,omitempty"`
	Confidential     bool   `json:"confidential,omitempty"`
	Generation       int    `json:"generation"`
	Stage            int    `json:"stage"`
	CutMsgIdx        int    `json:"cut_msg_idx"`
	ArchivedMsgCount int    `json:"archived_msg_count"`
	OriginalTokens   int    `json:"original_tokens"`
	Stage1Tokens     int    `json:"stage1_tokens"`
	SummaryTokens    int    `json:"summary_tokens,omitempty"`
	Stage2Status     string `json:"stage2_status"`
	Stage2Backend    string `json:"stage2_backend,omitempty"`
	Stage2Ms         int64  `json:"stage2_ms,omitempty"`
	Stage2Err        string `json:"stage2_err,omitempty"`
	PrewarmPrompt    int    `json:"prewarm_prompt,omitempty"`
	PrewarmCached    int    `json:"prewarm_cached,omitempty"`
	Hits             int64  `json:"hits"`
	SummaryPreview   string `json:"summary_preview,omitempty"`
	CreatedAt        string `json:"created_at"`
	LastUsed         string `json:"last_used"`
}

type stage2Job struct {
	checkpointID      string
	sessionRoot       string
	originTarget      string
	confidential      bool
	model             string
	sysMsgs           []json.RawMessage
	tools             json.RawMessage
	extraOpts         map[string]json.RawMessage
	sourceHistoryMsgs []json.RawMessage
	sourceEstTokens   int
	prevConstraints   string
	nextMsgRole       string
	archivedCount     int
	originalTokens    int
	deferAfterLive    bool
}

type CompressResult struct {
	Body             []byte
	Compressed       bool
	Mode             string // "none", "stage1", "stage2"
	CheckpointID     string
	Stage2Status     string // "none", "pending", "running", "ready", "failed"
	SkipReason       string
	OriginalTokens   int
	CompressedTokens int
	PendingJob       *stage2Job
}

type ContextCompressor struct {
	router *Router

	cfgMu sync.RWMutex
	cfg   ContextCompressConfig

	persistMu sync.Mutex // serializes snapshot + temp write + rename; lock order: persistMu -> mu

	mu            sync.RWMutex
	checkpoints   map[string]*ContextCheckpoint
	bySessionRoot map[string][]*ContextCheckpoint
	inFlightSess  map[string]bool
	dirty         bool
	dirtySeq      uint64

	targetJobMu   sync.Mutex
	nonExtAbortCh chan struct{}

	loopMu     sync.Mutex
	loopCancel context.CancelFunc
	loopWG     sync.WaitGroup

	jobCh               chan *stage2Job
	stage2Active        atomic.Bool
	totalStage1Hits     atomic.Int64
	totalStage2Hits     atomic.Int64
	totalStage2Jobs     atomic.Int64
	totalStage2Fail     atomic.Int64
	totalSavedToks      atomic.Int64
	deferAfterLiveDelay atomic.Int64 // nanoseconds; 0 defaults to 1500ms, <0 disables
	idlePollInterval    atomic.Int64 // nanoseconds; <=0 defaults to 2s
}

func newContextCompressor(cfg ContextCompressConfig, router *Router) *ContextCompressor {
	cm := &ContextCompressor{
		cfg:           cfg,
		router:        router,
		checkpoints:   make(map[string]*ContextCheckpoint),
		bySessionRoot: make(map[string][]*ContextCheckpoint),
		inFlightSess:  make(map[string]bool),
		nonExtAbortCh: make(chan struct{}),
		jobCh:         make(chan *stage2Job, 16),
	}
	cm.loadFromDisk(cfg.PersistPath)
	return cm
}

func (cm *ContextCompressor) getCfg() ContextCompressConfig {
	cm.cfgMu.RLock()
	c := cm.cfg
	cm.cfgMu.RUnlock()
	return c
}

func (cm *ContextCompressor) applyConfig(next ContextCompressConfig) {
	cm.cfgMu.Lock()
	oldPath := cm.cfg.PersistPath
	cm.cfg = next
	cm.cfgMu.Unlock()

	if oldPath != next.PersistPath {
		if oldPath != "" {
			cm.saveToPath(oldPath)
		}
		if next.PersistPath != "" {
			cm.loadFromDisk(next.PersistPath)
		}
	}
}

func (cm *ContextCompressor) markDirtyLocked() {
	cm.dirty = true
	cm.dirtySeq++
}

func (cm *ContextCompressor) checkJobTrustBoundary(job *stage2Job) (string, error) {
	routerTarget := cm.router.getTarget()
	if routerTarget != "local" && (job.confidential || normalizeOriginTarget(job.originTarget) != "external") {
		return routerTarget, errors.New("trust boundary changed: non-external checkpoint cannot run on external target")
	}
	return routerTarget, nil
}

func (cm *ContextCompressor) hasCheckpoint(id string) bool {
	cm.mu.RLock()
	_, ok := cm.checkpoints[id]
	cm.mu.RUnlock()
	return ok
}

func (cm *ContextCompressor) purgeCheckpointOnBoundaryChange(job *stage2Job) {
	shouldSave := false
	cm.mu.Lock()
	if cur, ok := cm.checkpoints[job.checkpointID]; ok {
		delete(cm.checkpoints, job.checkpointID)
		cm.rebuildSessionIndexLocked()
		if !cur.Confidential {
			cm.markDirtyLocked()
			shouldSave = true
		}
	}
	cm.mu.Unlock()
	if shouldSave {
		cm.saveToDisk()
	}
}

func (cm *ContextCompressor) enforceTarget(routerTarget string) {
	if routerTarget == "local" {
		return
	}
	cm.targetJobMu.Lock()
	close(cm.nonExtAbortCh)
	cm.nonExtAbortCh = make(chan struct{})
	cm.targetJobMu.Unlock()

	cm.mu.Lock()
	removedPersisted := 0
	for id, cp := range cm.checkpoints {
		if cp.Confidential || normalizeOriginTarget(cp.OriginTarget) != "external" {
			delete(cm.checkpoints, id)
			if !cp.Confidential {
				removedPersisted++
			}
		}
	}
	if removedPersisted > 0 {
		cm.rebuildSessionIndexLocked()
		cm.markDirtyLocked()
	} else {
		cm.rebuildSessionIndexLocked()
	}
	cm.mu.Unlock()
	if removedPersisted > 0 {
		cm.router.log.Info("purged non-external context checkpoints after target change", "provider", cm.router.name, "removed", removedPersisted)
		cm.saveToDisk()
	}
}

func (cm *ContextCompressor) syncLoop(parent context.Context, shouldRun bool) {
	cm.loopMu.Lock()
	if !shouldRun {
		cancel := cm.loopCancel
		cm.loopCancel = nil
		cm.loopMu.Unlock()
		if cancel != nil {
			cancel()
		}
		cm.loopWG.Wait()
		return
	}
	if cm.loopCancel != nil {
		cm.loopMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	cm.loopCancel = cancel
	cm.loopWG.Add(2)
	cm.loopMu.Unlock()

	go func() {
		defer cm.loopWG.Done()
		cm.stage2WorkerLoop(ctx)
	}()
	go func() {
		defer cm.loopWG.Done()
		tk := time.NewTicker(60 * time.Second)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				cm.gcAndSave()
			}
		}
	}()
}

// ScheduleStage2 enqueues a background Stage 2 summarization job (non-blocking, deduplicated per session).
func (cm *ContextCompressor) ScheduleStage2(job *stage2Job) {
	cfg := cm.getCfg()
	if !cfg.Enabled || !stage2Enabled(cfg) || job == nil {
		return
	}
	upgraded := false
	cm.mu.Lock()
	if cp, ok := cm.checkpoints[job.checkpointID]; ok && job.confidential && !cp.Confidential {
		cp.Confidential = true
		cm.markDirtyLocked()
		upgraded = true
	}
	if cm.inFlightSess[job.sessionRoot] {
		cm.mu.Unlock()
		if upgraded {
			cm.saveToDisk()
		}
		return
	}
	if cp, ok := cm.checkpoints[job.checkpointID]; ok {
		if cp.Stage == 2 && cp.Stage2Status == "ready" {
			cm.mu.Unlock()
			if upgraded {
				cm.saveToDisk()
			}
			return
		}
		if cp.Stage2Status == "failed" && !cp.Stage2FailedAt.IsZero() && time.Since(cp.Stage2FailedAt) < stage2RetryCooldown {
			cm.mu.Unlock()
			if upgraded {
				cm.saveToDisk()
			}
			return
		}
	}
	cm.inFlightSess[job.sessionRoot] = true
	cm.mu.Unlock()
	if upgraded {
		cm.saveToDisk()
	}

	select {
	case cm.jobCh <- job:
		cm.mu.Lock()
		if cp, ok := cm.checkpoints[job.checkpointID]; ok && cp.Stage < 2 && cp.Stage2Status != "running" && cp.Stage2Status != "ready" {
			cp.Stage2Status = "pending"
		}
		cm.mu.Unlock()
		cm.router.log.Info("context compress stage2 queued", "provider", cm.router.name, "id", shortID(job.checkpointID), "sess", shortID(job.sessionRoot), "src_est", job.sourceEstTokens)
	default:
		cm.mu.Lock()
		delete(cm.inFlightSess, job.sessionRoot)
		cm.mu.Unlock()
		cm.router.log.Warn("context compress stage2 queue full, skipped", "provider", cm.router.name, "id", shortID(job.checkpointID))
	}
}

// MaybeCompress inspects an incoming OpenAI chat completion request body.
// If it matches an existing frozen checkpoint OR exceeds trigger_tokens, returns compressed body & metadata.
func (cm *ContextCompressor) MaybeCompress(
	body []byte,
	rawAn *RequestAnalysis,
	headers http.Header,
	cfg ContextCompressConfig,
	heavyMissThreshold int,
	originTarget string,
	confidential bool,
) CompressResult {
	return cm.MaybeCompressWithScope(body, rawAn, headers, cfg, heavyMissThreshold, originTarget, confidential, "")
}

// MaybeCompressWithScope behaves like MaybeCompress while scoping checkpoints by
// confidential status, caller scope, and canonical system messages.
func (cm *ContextCompressor) MaybeCompressWithScope(
	body []byte,
	rawAn *RequestAnalysis,
	headers http.Header,
	cfg ContextCompressConfig,
	heavyMissThreshold int,
	originTarget string,
	confidential bool,
	callerScope string,
) CompressResult {
	res := CompressResult{
		Body:           body,
		Mode:           "none",
		Stage2Status:   "none",
		OriginalTokens: 0,
	}
	if rawAn != nil {
		res.OriginalTokens = rawAn.TotalEst
	}
	if !cfg.Enabled || rawAn == nil {
		res.SkipReason = "disabled"
		return res
	}
	if isBypassRequested(headers) {
		res.SkipReason = "bypass-header"
		return res
	}
	if heavyMissThreshold <= 0 {
		heavyMissThreshold = 24000
	}
	originTarget = normalizeOriginTarget(originTarget)

	// Fast-path check: if below trigger AND no checkpoints exist in memory, return immediately with zero JSON work.
	cm.mu.RLock()
	hasAnyCP := len(cm.checkpoints) > 0
	cm.mu.RUnlock()
	if rawAn.TotalEst < cfg.TriggerTokens && !hasAnyCP {
		res.SkipReason = "below-threshold"
		return res
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		res.SkipReason = "parse-error"
		return res
	}
	rawMsgsField, ok := top["messages"]
	if !ok {
		res.SkipReason = "no-messages"
		return res
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgsField, &msgs); err != nil || len(msgs) == 0 {
		res.SkipReason = "no-messages"
		return res
	}

	sysEnd := findLeadingSystemEnd(msgs)
	if sysEnd >= len(msgs) {
		res.SkipReason = "only-system"
		return res
	}

	sysMsgs := msgs[:sysEnd]
	scopeHash := computeCheckpointScopeHash(originTarget, confidential, callerScope, sysMsgs)
	hashSeed := "body-v2|" + scopeHash

	// O(1) session root check when below trigger threshold
	sessionRoot := computeSingleBodyHash(hashSeed, msgs[sysEnd])
	cm.mu.RLock()
	hasSessionCP := len(cm.bySessionRoot[sessionRoot]) > 0
	cm.mu.RUnlock()

	if rawAn.TotalEst < cfg.TriggerTokens && !hasSessionCP {
		res.SkipReason = "below-threshold"
		return res
	}

	// Compute cumulative body hashes [sysEnd..len(msgs)-1] seeded with caller/security scope + system messages.
	bodyHashes := computeBodyHashes(hashSeed, msgs, sysEnd)

	// 1. Check for deepest matching existing checkpoint (longest prefix match within trust & caller boundary)
	var matchedCP *ContextCheckpoint
	matchedCutIdx := -1
	if hasSessionCP {
		cm.mu.RLock()
		for i := len(msgs) - 1; i >= sysEnd; i-- {
			if cp, ok := cm.checkpoints[bodyHashes[i]]; ok {
				if cp.Confidential != confidential {
					continue
				}
				if cp.ScopeHash != scopeHash {
					continue
				}
				if originTarget != "local" && (cp.Confidential || normalizeOriginTarget(cp.OriginTarget) != "external") {
					continue
				}
				cpCopy := *cp
				matchedCP = &cpCopy
				matchedCutIdx = i
				break
			}
		}
		cm.mu.RUnlock()
	}

	toolsRaw := top["tools"]
	toolsEst := estimateToolsTokens(toolsRaw)
	sysEst := estimateMessagesTokens(sysMsgs)
	model := extractModelName(top)
	extraOpts := extractPrefixExtraOpts(top)

	// Case A: Matched an existing checkpoint!
	if matchedCP != nil && matchedCutIdx < len(msgs)-1 {
		tailMsgs := msgs[matchedCutIdx+1:]
		tailEst := estimateMessagesTokens(tailMsgs)
		effectiveConfidential := confidential || matchedCP.Confidential
		if effectiveConfidential {
			cm.upgradeConfidential(matchedCP.ID)
		}

		// Stage 2 ready checkpoint!
		if matchedCP.Stage == 2 && matchedCP.SummaryText != "" {
			nextRole := extractMessageRole(tailMsgs[0])
			injected := buildStage2InjectedMessages(matchedCP, nextRole)
			injectedEst := estimateMessagesTokens(injected)
			effectiveTotal := sysEst + toolsEst + injectedEst + tailEst

			// If tail alone + summary grew past trigger_tokens again -> trigger Generation N+1 re-compression!
			if effectiveTotal < cfg.TriggerTokens {
				newBody, err := rebuildBodyWithMessages(top, sysMsgs, injected, tailMsgs)
				if err == nil {
					cm.touchCheckpoint(matchedCP.ID)
					cm.totalStage2Hits.Add(1)
					if rawAn.TotalEst > effectiveTotal {
						cm.totalSavedToks.Add(int64(rawAn.TotalEst - effectiveTotal))
					}
					res.Body = newBody
					res.Compressed = true
					res.Mode = "stage2"
					res.CheckpointID = matchedCP.ID
					res.Stage2Status = "ready"
					res.CompressedTokens = effectiveTotal
					return res
				}
			} else {
				return cm.compressAndRegister(body, top, msgs, sysEnd, bodyHashes, sessionRoot, scopeHash, rawAn,
					sysMsgs, toolsRaw, extraOpts, model, sysEst, toolsEst,
					cfg, heavyMissThreshold, originTarget, effectiveConfidential,
					matchedCP, injected, tailMsgs, matchedCutIdx+1)
			}
		}

		// Stage 1 frozen checkpoint (while Stage 2 is running/pending/disabled)
		if matchedCP.Stage == 1 && len(matchedCP.Stage1Msgs) > 0 {
			stage1Est := estimateMessagesTokens(matchedCP.Stage1Msgs)
			effectiveTotal := sysEst + toolsEst + stage1Est + tailEst

			if effectiveTotal < cfg.TriggerTokens {
				newBody, err := rebuildBodyWithMessages(top, sysMsgs, matchedCP.Stage1Msgs, tailMsgs)
				if err == nil {
					cm.touchCheckpoint(matchedCP.ID)
					cm.totalStage1Hits.Add(1)
					if rawAn.TotalEst > effectiveTotal {
						cm.totalSavedToks.Add(int64(rawAn.TotalEst - effectiveTotal))
					}
					res.Body = newBody
					res.Compressed = true
					res.Mode = "stage1"
					res.CheckpointID = matchedCP.ID
					res.Stage2Status = matchedCP.Stage2Status
					res.CompressedTokens = effectiveTotal
					if cm.shouldScheduleStage2(cfg, matchedCP, sessionRoot) {
						res.PendingJob = &stage2Job{
							checkpointID:      matchedCP.ID,
							sessionRoot:       sessionRoot,
							originTarget:      originTarget,
							confidential:      effectiveConfidential,
							model:             model,
							sysMsgs:           cloneRawMessages(sysMsgs),
							tools:             cloneRawBytes(toolsRaw),
							extraOpts:         cloneExtraOpts(extraOpts),
							sourceHistoryMsgs: cloneRawMessages(matchedCP.Stage1Msgs),
							sourceEstTokens:   sysEst + toolsEst + stage1Est,
							prevConstraints:   matchedCP.HardConstraints,
							nextMsgRole:       extractMessageRole(tailMsgs[0]),
							archivedCount:     matchedCP.ArchivedMsgCount,
							originalTokens:    matchedCP.OriginalTokens,
							deferAfterLive:    true,
						}
						res.Stage2Status = "pending"
					}
					return res
				}
			} else {
				return cm.compressAndRegister(body, top, msgs, sysEnd, bodyHashes, sessionRoot, scopeHash, rawAn,
					sysMsgs, toolsRaw, extraOpts, model, sysEst, toolsEst,
					cfg, heavyMissThreshold, originTarget, effectiveConfidential,
					matchedCP, matchedCP.Stage1Msgs, tailMsgs, matchedCutIdx+1)
			}
		}

		// Stage 0 placeholder (warm pass-through waiting for Stage 2): pass through ONLY while still warm;
		// if cache dropped mid-wait, fall through to Stage 1 immediately.
		isStillWarm := rawAn.CachedEst > 0 && rawAn.CachedEst*2 >= rawAn.TotalEst && rawAn.MissEst < heavyMissThreshold*2
		if matchedCP.Stage == 0 && isStillWarm && rawAn.TotalEst < cfg.TriggerTokens*12/10 {
			res.CheckpointID = matchedCP.ID
			res.Stage2Status = matchedCP.Stage2Status
			res.SkipReason = "stage2-in-progress"
			if cm.shouldScheduleStage2(cfg, matchedCP, sessionRoot) {
				res.PendingJob = &stage2Job{
					checkpointID:      matchedCP.ID,
					sessionRoot:       sessionRoot,
					originTarget:      originTarget,
					confidential:      effectiveConfidential,
					model:             model,
					sysMsgs:           cloneRawMessages(sysMsgs),
					tools:             cloneRawBytes(toolsRaw),
					extraOpts:         cloneExtraOpts(extraOpts),
					sourceHistoryMsgs: cloneRawMessages(msgs[sysEnd : matchedCutIdx+1]),
					sourceEstTokens:   sysEst + toolsEst + estimateMessagesTokens(msgs[sysEnd:matchedCutIdx+1]),
					prevConstraints:   matchedCP.HardConstraints,
					nextMsgRole:       extractMessageRole(tailMsgs[0]),
					archivedCount:     matchedCP.ArchivedMsgCount,
					originalTokens:    matchedCP.OriginalTokens,
					deferAfterLive:    true,
				}
				res.Stage2Status = "pending"
			}
			return res
		}
	}

	// Case B: No matching checkpoint yet — only compress if >= trigger_tokens
	if rawAn.TotalEst < cfg.TriggerTokens {
		res.SkipReason = "below-threshold"
		return res
	}

	return cm.compressAndRegister(body, top, msgs, sysEnd, bodyHashes, sessionRoot, scopeHash, rawAn,
		sysMsgs, toolsRaw, extraOpts, model, sysEst, toolsEst,
		cfg, heavyMissThreshold, originTarget, confidential,
		nil, nil, msgs[sysEnd:], sysEnd)
}

func (cm *ContextCompressor) shouldScheduleStage2(cfg ContextCompressConfig, cp *ContextCheckpoint, sessionRoot string) bool {
	if !stage2Enabled(cfg) || cp == nil || cp.Stage == 2 {
		return false
	}
	cm.mu.RLock()
	busy := cm.inFlightSess[sessionRoot]
	cm.mu.RUnlock()
	if busy {
		return false
	}
	if cp.Stage2Status == "none" || cp.Stage2Status == "pending" {
		return true
	}
	return cp.Stage2Status == "failed" && time.Since(cp.Stage2FailedAt) >= stage2RetryCooldown
}

func (cm *ContextCompressor) resetPendingStage2Status(checkpointID string) {
	cm.mu.Lock()
	if cp, ok := cm.checkpoints[checkpointID]; ok && cp.Stage < 2 && (cp.Stage2Status == "pending" || cp.Stage2Status == "running") {
		cp.Stage2Status = "none"
	}
	cm.mu.Unlock()
}

func (cm *ContextCompressor) compressAndRegister(
	origBody []byte,
	top map[string]json.RawMessage,
	origMsgs []json.RawMessage,
	sysEnd int,
	bodyHashes []string,
	sessionRoot string,
	scopeHash string,
	rawAn *RequestAnalysis,
	sysMsgs []json.RawMessage,
	toolsRaw json.RawMessage,
	extraOpts map[string]json.RawMessage,
	model string,
	sysEst int,
	toolsEst int,
	cfg ContextCompressConfig,
	heavyMissThreshold int,
	originTarget string,
	confidential bool,
	prevCP *ContextCheckpoint,
	prevPrefixMsgs []json.RawMessage,
	workingMsgs []json.RawMessage,
	workingOrigOffset int,
) CompressResult {
	res := CompressResult{
		Body:           origBody,
		Mode:           "none",
		Stage2Status:   "none",
		OriginalTokens: rawAn.TotalEst,
	}

	// Group workingMsgs into atomic turns (never split assistant(tool_calls) from its tool replies)
	groups := groupAtomicTurns(workingMsgs)
	if len(groups) <= 2 {
		res.SkipReason = "too-few-turns"
		return res
	}

	cutGroupEnd := chooseCutGroupEnd(cfg, groups)
	if cutGroupEnd <= 0 || cutGroupEnd >= len(groups) {
		res.SkipReason = "no-valid-cut"
		return res
	}

	cutWorkIdx := groups[cutGroupEnd-1].endIdx
	origCutIdx := workingOrigOffset + cutWorkIdx
	if origCutIdx >= len(origMsgs)-1 {
		res.SkipReason = "no-tail-left"
		return res
	}

	tailMsgs := workingMsgs[cutWorkIdx+1:]
	if len(tailMsgs) == 0 {
		res.SkipReason = "no-tail-left"
		return res
	}

	histRaw := make([]json.RawMessage, 0, len(prevPrefixMsgs)+cutWorkIdx+1)
	protectedPrefixLen := len(prevPrefixMsgs)
	histRaw = append(histRaw, prevPrefixMsgs...)
	histRaw = append(histRaw, workingMsgs[:cutWorkIdx+1]...)

	tailEst := estimateMessagesTokens(tailMsgs)
	stage1Budget := cfg.Stage1TargetTokens - sysEst - toolsEst - tailEst
	if stage1Budget < 16000 {
		stage1Budget = 16000
	}
	stage1Hist := stage1PruneHistory(cfg, histRaw, tailMsgs, protectedPrefixLen, stage1Budget)
	stage1HistEst := estimateMessagesTokens(stage1Hist)
	stage1TotalEst := sysEst + toolsEst + stage1HistEst + tailEst

	cpID := bodyHashes[origCutIdx]
	nextRole := extractMessageRole(tailMsgs[0])
	archivedCount := origCutIdx - sysEnd + 1
	gen := 1
	prevConstraints := ""
	effectiveConfidential := confidential
	if prevCP != nil {
		gen = prevCP.Generation + 1
		prevConstraints = prevCP.HardConstraints
		if prevCP.Confidential {
			effectiveConfidential = true
		}
	}

	// Warm request check: >=50% cached and miss below 2x heavy threshold.
	isWarm := rawAn.CachedEst > 0 && rawAn.CachedEst*2 >= rawAn.TotalEst && rawAn.MissEst < heavyMissThreshold*2
	dropPct := 0
	if rawAn.TotalEst > 0 {
		dropPct = (rawAn.TotalEst - stage1TotalEst) * 100 / rawAn.TotalEst
	}

	applyStage1Now := false
	if !isWarm {
		if dropPct >= 10 {
			applyStage1Now = true
		}
	} else {
		if stage1TotalEst < cfg.Stage2WarmStage1MaxTokens && dropPct >= cfg.Stage2WarmStage1MinDrop {
			applyStage1Now = true
		}
	}

	now := time.Now()
	cp := &ContextCheckpoint{
		ID:               cpID,
		SessionRoot:      sessionRoot,
		OriginTarget:     normalizeOriginTarget(originTarget),
		ScopeHash:        scopeHash,
		Confidential:     effectiveConfidential,
		CutMsgIdx:        origCutIdx,
		ArchivedMsgCount: archivedCount,
		NextMsgRole:      nextRole,
		Generation:       gen,
		OriginalTokens:   rawAn.TotalEst,
		Stage1Tokens:     stage1TotalEst,
		HardConstraints:  prevConstraints,
		Stage2Status:     "none",
		CreatedAt:        now,
		LastUsed:         now,
	}

	if applyStage1Now {
		newBody, err := rebuildBodyWithMessages(top, sysMsgs, stage1Hist, tailMsgs)
		if err != nil {
			res.SkipReason = "marshal-error"
			return res
		}
		cp.Stage = 1
		cp.Stage1Msgs = cloneRawMessages(stage1Hist)
		cp.Hits = 1
		cm.upsertCheckpoint(cp)
		cm.totalStage1Hits.Add(1)
		if rawAn.TotalEst > stage1TotalEst {
			cm.totalSavedToks.Add(int64(rawAn.TotalEst - stage1TotalEst))
		}

		res.Body = newBody
		res.Compressed = true
		res.Mode = "stage1"
		res.CheckpointID = cpID
		res.CompressedTokens = stage1TotalEst
		if stage2Enabled(cfg) {
			res.Stage2Status = "pending"
			res.PendingJob = &stage2Job{
				checkpointID:      cpID,
				sessionRoot:       sessionRoot,
				originTarget:      cp.OriginTarget,
				confidential:      effectiveConfidential,
				model:             model,
				sysMsgs:           cloneRawMessages(sysMsgs),
				tools:             cloneRawBytes(toolsRaw),
				extraOpts:         cloneExtraOpts(extraOpts),
				sourceHistoryMsgs: cloneRawMessages(stage1Hist),
				sourceEstTokens:   sysEst + toolsEst + stage1HistEst,
				prevConstraints:   prevConstraints,
				nextMsgRole:       nextRole,
				archivedCount:     archivedCount,
				originalTokens:    rawAn.TotalEst,
				deferAfterLive:    true,
			}
		}
		cm.router.log.Info("context compress stage1 applied",
			"provider", cm.router.name, "id", shortID(cpID), "gen", gen, "warm", isWarm,
			"orig", rawAn.TotalEst, "comp", stage1TotalEst, "drop_pct", dropPct, "cut", origCutIdx)
		return res
	}

	if stage2Enabled(cfg) {
		cp.Stage = 0
		cm.upsertCheckpoint(cp)

		srcMsgs := histRaw
		srcEst := sysEst + toolsEst + estimateMessagesTokens(histRaw)
		if !isWarm && stage1HistEst < estimateMessagesTokens(histRaw) {
			srcMsgs = stage1Hist
			srcEst = sysEst + toolsEst + stage1HistEst
		}
		res.CheckpointID = cpID
		res.Stage2Status = "pending"
		res.SkipReason = "warm-passthrough-stage2-queued"
		res.PendingJob = &stage2Job{
			checkpointID:      cpID,
			sessionRoot:       sessionRoot,
			originTarget:      cp.OriginTarget,
			confidential:      effectiveConfidential,
			model:             model,
			sysMsgs:           cloneRawMessages(sysMsgs),
			tools:             cloneRawBytes(toolsRaw),
			extraOpts:         cloneExtraOpts(extraOpts),
			sourceHistoryMsgs: cloneRawMessages(srcMsgs),
			sourceEstTokens:   srcEst,
			prevConstraints:   prevConstraints,
			nextMsgRole:       nextRole,
			archivedCount:     archivedCount,
			originalTokens:    rawAn.TotalEst,
			deferAfterLive:    true,
		}
		cm.router.log.Info("context compress warm passthrough, queued stage2",
			"provider", cm.router.name, "id", shortID(cpID), "gen", gen,
			"orig", rawAn.TotalEst, "stage1", stage1TotalEst, "drop_pct", dropPct)
	} else {
		res.SkipReason = "stage1-insufficient-savings"
	}
	return res
}

// ---------- Atomic Turn Grouping & Cut Selection ----------

type atomicGroup struct {
	startIdx int
	endIdx   int
	estToks  int
	role     string
}

func groupAtomicTurns(msgs []json.RawMessage) []atomicGroup {
	var groups []atomicGroup
	n := len(msgs)
	i := 0
	for i < n {
		role := extractMessageRole(msgs[i])
		start := i
		toks := estimateMessageTokens(msgs[i])
		i++
		if role == "assistant" {
			for i < n && extractMessageRole(msgs[i]) == "tool" {
				toks += estimateMessageTokens(msgs[i])
				i++
			}
		}
		groups = append(groups, atomicGroup{
			startIdx: start,
			endIdx:   i - 1,
			estToks:  toks,
			role:     role,
		})
	}
	return groups
}

func chooseCutGroupEnd(cfg ContextCompressConfig, groups []atomicGroup) int {
	n := len(groups)
	keepCount := cfg.KeepRecentGroups
	if keepCount >= n-1 {
		keepCount = n - 2
	}
	if keepCount < 2 {
		keepCount = 2
	}
	cut := n - keepCount

	tailToks := 0
	for i := cut; i < n; i++ {
		tailToks += groups[i].estToks
	}

	for cut > 2 && tailToks < cfg.KeepRecentMinTokens {
		cut--
		tailToks += groups[cut].estToks
	}

	for cut < n-2 && tailToks > cfg.KeepRecentMaxTokens {
		tailToks -= groups[cut].estToks
		cut++
	}

	if cut > 2 && groups[cut].role != "user" && groups[cut-1].role == "user" {
		if tailToks+groups[cut-1].estToks <= cfg.KeepRecentMaxTokens {
			cut--
		}
	}
	if cut <= 0 || cut >= n {
		return 0
	}
	return cut
}

// ---------- Stage 1 Weighted Deterministic Pruning (P0 / P1 / P2 / P3) ----------

type toolCallMeta struct {
	name       string
	target     string
	isMutation bool
	isStale    bool
	superseded bool
}

type parsedHistoryMsg struct {
	raw        json.RawMessage
	obj        map[string]json.RawMessage
	role       string
	toolCallID string
	text       string
	hasImages  bool
	isP0       bool
	isP1       bool
	isError    bool
	meta       *toolCallMeta
}

func stage1PruneHistory(
	cfg ContextCompressConfig,
	histRaw []json.RawMessage,
	tailMsgs []json.RawMessage,
	protectedPrefixLen int,
	targetBudgetTokens int,
) []json.RawMessage {
	n := len(histRaw)
	if n == 0 {
		return nil
	}

	metaByID := make(map[string]*toolCallMeta)
	seenTargets := make(map[string]bool)
	for _, rm := range tailMsgs {
		for _, tc := range extractToolCalls(rm) {
			if tc.target != "" {
				seenTargets[tc.target] = true
			}
		}
	}

	items := make([]*parsedHistoryMsg, n)
	for i, rm := range histRaw {
		pm := parseHistoryMsg(rm)
		if i < protectedPrefixLen || i == protectedPrefixLen {
			pm.isP0 = true
		}
		if pm.role == "assistant" {
			for _, tc := range extractToolCalls(rm) {
				tcCopy := tc
				metaByID[tc.id] = &toolCallMeta{
					name:       tcCopy.name,
					target:     tcCopy.target,
					isMutation: tcCopy.isMutation,
				}
			}
		}
		items[i] = pm
	}

	for _, pm := range items {
		if pm.role == "tool" && pm.toolCallID != "" {
			if m, ok := metaByID[pm.toolCallID]; ok {
				pm.meta = m
			}
		}
		pm.isError = hasErrorSignal(pm.text)
		if pm.role == "user" && hasConstraintSignal(pm.text) {
			pm.isP1 = true
		}
		if pm.role == "tool" && (pm.isError || (pm.meta != nil && pm.meta.isMutation)) {
			pm.isP1 = true
		}
	}

	succeededToolKey := make(map[string]bool)
	for i := n - 1; i >= 0; i-- {
		pm := items[i]
		if pm.role != "tool" || pm.meta == nil {
			continue
		}
		m := pm.meta
		if !m.isMutation && m.target != "" {
			if seenTargets[m.target] {
				m.isStale = true
			} else {
				seenTargets[m.target] = true
			}
		}
		tKey := m.name + "|" + m.target
		if m.name != "" {
			if !pm.isError {
				succeededToolKey[tKey] = true
			} else if succeededToolKey[tKey] && m.target != "" {
				m.superseded = true
			}
		}
	}

	out := make([]json.RawMessage, n)
	estToks := 0
	for i, pm := range items {
		out[i] = pruneSingleMessage(cfg, pm, 1.0)
		estToks += estimateMessageTokens(out[i])
	}

	if estToks > targetBudgetTokens {
		type scoredIdx struct {
			idx   int
			score int
		}
		candidates := make([]scoredIdx, 0, n)
		for i, pm := range items {
			if pm.isP0 {
				continue
			}
			score := computeMessageImportanceScore(pm, i, n)
			candidates = append(candidates, scoredIdx{idx: i, score: score})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].score != candidates[j].score {
				return candidates[i].score < candidates[j].score
			}
			return candidates[i].idx < candidates[j].idx
		})

		for _, scale := range []float64{0.45, 0.20} {
			if estToks <= targetBudgetTokens {
				break
			}
			for _, c := range candidates {
				if estToks <= targetBudgetTokens {
					break
				}
				pm := items[c.idx]
				if scale < 0.3 && pm.role == "user" && pm.isP1 && len(pm.text) < 4000 {
					continue
				}
				oldTok := estimateMessageTokens(out[c.idx])
				tight := pruneSingleMessage(cfg, pm, scale)
				newTok := estimateMessageTokens(tight)
				if newTok < oldTok {
					out[c.idx] = tight
					estToks -= (oldTok - newTok)
				}
			}
		}
	}

	return out
}

func computeMessageImportanceScore(pm *parsedHistoryMsg, idx, total int) int {
	score := 0
	switch pm.role {
	case "user":
		score = 45
	case "assistant":
		score = 30
	case "tool":
		if pm.meta != nil && pm.meta.isMutation {
			score = 35
		} else if pm.meta != nil && (pm.meta.isStale || pm.meta.superseded) {
			score = 2
		} else {
			score = 12
		}
	default:
		score = 25
	}
	if pm.isP1 {
		score += 30
	}
	if pm.isError {
		score += 20
	}
	if pm.role == "assistant" && bytes.Contains(pm.raw, []byte(`"tool_calls"`)) {
		score += 10
	}
	if total > 1 {
		score += (idx * 20) / (total - 1)
	}
	return score
}

func pruneSingleMessage(cfg ContextCompressConfig, pm *parsedHistoryMsg, budgetScale float64) json.RawMessage {
	if pm.obj == nil {
		return pm.raw
	}
	obj := make(map[string]json.RawMessage, len(pm.obj))
	for k, v := range pm.obj {
		obj[k] = v
	}
	changed := false

	if pm.role == "assistant" {
		if _, ok := obj["reasoning_content"]; ok {
			delete(obj, "reasoning_content")
			changed = true
		}
		if _, ok := obj["reasoning"]; ok {
			delete(obj, "reasoning")
			changed = true
		}
	}

	maxChars := 0
	headRatio := 0.55
	tailRatio := 0.35
	maxErrLines := 6
	suffixNote := ""

	switch pm.role {
	case "tool":
		if pm.meta != nil && pm.meta.superseded {
			maxChars = 300
			suffixNote = "[此前同类工具尝试失败且后续已成功，过程已折叠]"
		} else if pm.meta != nil && pm.meta.isStale {
			maxChars = cfg.StaleReadMaxChars
			suffixNote = fmt.Sprintf("[旧只读快照已过期，后续已重读/修改 %s]", pm.meta.target)
		} else if pm.isP1 {
			maxChars = cfg.MutationToolMaxChars
			maxErrLines = 8
			suffixNote = "[状态变更/异常工具输出已按高权保留首尾与报错行]"
		} else {
			maxChars = cfg.ToolMaxChars
			suffixNote = "[只读工具结果已折叠，需要细节可重新调用工具]"
		}
	case "user":
		if pm.isP0 {
			maxChars = cfg.UserPasteMaxChars
			if budgetScale < 1.0 {
				budgetScale = 1.0
			}
			suffixNote = "[首轮超长粘贴块已折叠中间部分]"
		} else if pm.isP1 {
			maxChars = cfg.UserPasteMaxChars * 2
			suffixNote = "[历史用户约束消息超长粘贴已折叠]"
		} else {
			maxChars = cfg.UserPasteMaxChars
			suffixNote = "[历史用户超长粘贴已折叠]"
		}
	case "assistant":
		if pm.isP0 {
			maxChars = cfg.AssistantMaxChars * 2
			if budgetScale < 1.0 {
				budgetScale = 1.0
			}
		} else {
			maxChars = cfg.AssistantMaxChars
			suffixNote = "[历史回复过长已保留首尾结论]"
		}
	default:
		maxChars = cfg.UserPasteMaxChars
	}

	if budgetScale < 1.0 && maxChars > 0 {
		maxChars = int(float64(maxChars) * budgetScale)
		if maxChars < 280 {
			maxChars = 280
		}
	}

	if rawContent, ok := obj["content"]; ok {
		newContent, contentChanged := pruneMessageContent(rawContent, maxChars, headRatio, tailRatio, maxErrLines, suffixNote)
		if contentChanged {
			obj["content"] = newContent
			changed = true
		}
	}

	if !changed {
		return pm.raw
	}
	encoded, err := marshalNoEscapeHTML(obj)
	if err != nil {
		return pm.raw
	}
	return encoded
}

func pruneMessageContent(raw json.RawMessage, maxRunes int, headRatio, tailRatio float64, maxErrLines int, note string) (json.RawMessage, bool) {
	c := bytes.TrimSpace(raw)
	if len(c) == 0 || bytes.Equal(c, []byte("null")) {
		return raw, false
	}

	if c[0] == '"' {
		var s string
		if err := json.Unmarshal(c, &s); err != nil {
			return raw, false
		}
		if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
			return raw, false
		}
		pruned := smartTruncateText(s, maxRunes, headRatio, tailRatio, maxErrLines, note)
		enc, err := marshalNoEscapeHTML(pruned)
		if err != nil {
			return raw, false
		}
		return enc, true
	}

	if c[0] == '[' {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(c, &parts); err != nil {
			return raw, false
		}
		changed := false
		outParts := make([]map[string]json.RawMessage, 0, len(parts))
		for _, p := range parts {
			var pType string
			if rawT, ok := p["type"]; ok {
				_ = json.Unmarshal(rawT, &pType)
			}
			if pType == "image_url" || pType == "input_image" || p["image_url"] != nil {
				outParts = append(outParts, map[string]json.RawMessage{
					"type": json.RawMessage(`"text"`),
					"text": json.RawMessage(`"[历史图片已省略]"`),
				})
				changed = true
				continue
			}
			if rawTxt, ok := p["text"]; ok {
				var s string
				if json.Unmarshal(rawTxt, &s) == nil && maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
					pruned := smartTruncateText(s, maxRunes, headRatio, tailRatio, maxErrLines, note)
					if enc, err := marshalNoEscapeHTML(pruned); err == nil {
						cp := make(map[string]json.RawMessage, len(p))
						for k, v := range p {
							cp[k] = v
						}
						cp["text"] = enc
						outParts = append(outParts, cp)
						changed = true
						continue
					}
				}
			}
			outParts = append(outParts, p)
		}
		if !changed {
			return raw, false
		}
		enc, err := marshalNoEscapeHTML(outParts)
		if err != nil {
			return raw, false
		}
		return enc, true
	}

	return raw, false
}

func smartTruncateText(s string, maxRunes int, headRatio, tailRatio float64, maxErrLines int, note string) string {
	runes := []rune(s)
	total := len(runes)
	if total <= maxRunes || maxRunes < 80 {
		return s
	}
	headLen := int(float64(maxRunes) * headRatio)
	tailLen := int(float64(maxRunes) * tailRatio)
	if headLen < 80 {
		headLen = 80
	}
	if tailLen < 60 {
		tailLen = 60
	}
	if headLen+tailLen >= total {
		return s
	}

	head := string(runes[:headLen])
	mid := string(runes[headLen : total-tailLen])
	tail := string(runes[total-tailLen:])
	omitted := total - headLen - tailLen

	errLines := extractErrorLines(mid, maxErrLines)

	var sb strings.Builder
	sb.WriteString(head)
	sb.WriteString("\n\n... [省略中间 ")
	sb.WriteString(fmt.Sprintf("%d", omitted))
	sb.WriteString(" 字符 (原文 ")
	sb.WriteString(fmt.Sprintf("%d", total))
	sb.WriteString(" 字符)")
	if note != "" {
		sb.WriteString(" · ")
		sb.WriteString(note)
	}
	sb.WriteString("] ...\n")
	if len(errLines) > 0 {
		sb.WriteString("[关键错误/异常行摘录]:\n")
		for _, el := range errLines {
			sb.WriteString("  ")
			sb.WriteString(el)
			sb.WriteByte('\n')
		}
	}
	sb.WriteByte('\n')
	sb.WriteString(tail)
	return sb.String()
}

var errorKeywords = []string{
	"error", "exception", "traceback", "panic:", "fatal", "failed", "failure",
	"exit code", "exit status", "errno", "permission denied", "oom", "timeout",
	"connection refused", "segmentation fault", "syntaxerror", "typeerror",
	"报错", "失败", "异常", "超时",
}

func hasErrorSignal(s string) bool {
	if len(s) == 0 {
		return false
	}
	low := strings.ToLower(s)
	for _, kw := range errorKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

var constraintKeywords = []string{
	"不要", "别动", "禁止", "严禁", "必须", "务必", "改成", "改为", "不对", "记住", "以后都",
	"never ", "must not", "do not ", "don't ", "instead ", "always use", "forbid",
}

func hasConstraintSignal(s string) bool {
	if len(s) == 0 {
		return false
	}
	low := strings.ToLower(s)
	for _, kw := range constraintKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func extractErrorLines(mid string, maxLines int) []string {
	if maxLines <= 0 || len(mid) == 0 {
		return nil
	}
	lines := strings.Split(mid, "\n")
	seen := make(map[string]bool)
	var out []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if len(t) < 4 {
			continue
		}
		if !hasErrorSignal(t) {
			continue
		}
		if len(t) > 180 {
			t = truncateUTF8(t, 180) + "..."
		}
		if seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
		if len(out) >= maxLines {
			break
		}
	}
	return out
}

type parsedToolCall struct {
	id         string
	name       string
	arguments  string
	target     string
	isMutation bool
}

func extractToolCalls(rawMsg json.RawMessage) []parsedToolCall {
	if !bytes.Contains(rawMsg, []byte(`"tool_calls"`)) {
		return nil
	}
	var msg struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(rawMsg, &msg); err != nil || len(msg.ToolCalls) == 0 {
		return nil
	}
	out := make([]parsedToolCall, 0, len(msg.ToolCalls))
	for _, tc := range msg.ToolCalls {
		target, cmdText := extractToolTargetAndCmd(tc.Function.Arguments)
		out = append(out, parsedToolCall{
			id:         tc.ID,
			name:       tc.Function.Name,
			arguments:  tc.Function.Arguments,
			target:     target,
			isMutation: isMutationTool(tc.Function.Name, cmdText),
		})
	}
	return out
}

func extractToolTargetAndCmd(argsJSON string) (target string, cmd string) {
	if argsJSON == "" {
		return "", ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return "", ""
	}
	for _, k := range []string{"file_path", "path", "Path", "AbsolutePath", "TargetFile", "target_file", "url", "uri", "URL", "DirectoryPath"} {
		if v, ok := m[k].(string); ok && v != "" {
			target = strings.TrimSpace(v)
			break
		}
	}
	for _, k := range []string{"command", "CommandLine", "cmd", "script"} {
		if v, ok := m[k].(string); ok && v != "" {
			cmd = strings.TrimSpace(v)
			break
		}
	}
	return target, cmd
}

func isMutationTool(toolName, cmd string) bool {
	low := strings.ToLower(toolName)
	for _, kw := range []string{"write", "edit", "patch", "replace", "create", "delete", "remove", "mkdir", "apply", "deploy", "commit", "push", "save", "update", "insert"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	for _, kw := range []string{"bash", "shell", "exec", "terminal", "command"} {
		if strings.Contains(low, kw) {
			if cmd == "" {
				return true
			}
			cl := strings.ToLower(cmd)
			for _, mut := range []string{
				" > ", ">>", "sed -i", "tee ", "rm ", "mv ", "cp ", "chmod ", "chown ",
				"systemctl ", "docker ", "apt ", "yum ", "pip ", "npm ", "go build", "make ",
				"git commit", "git push", "git checkout", "git reset", "git merge",
				"kubectl apply", "kubectl delete", "patch ", "mkdir ", "touch ",
			} {
				if strings.Contains(cl, mut) {
					return true
				}
			}
			return false
		}
	}
	return false
}

func parseHistoryMsg(raw json.RawMessage) *parsedHistoryMsg {
	pm := &parsedHistoryMsg{raw: raw}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return pm
	}
	pm.obj = obj
	if r, ok := obj["role"]; ok {
		_ = json.Unmarshal(r, &pm.role)
	}
	if id, ok := obj["tool_call_id"]; ok {
		_ = json.Unmarshal(id, &pm.toolCallID)
	}
	if c, ok := obj["content"]; ok {
		pm.text = extractRawContentText(c)
		pm.hasImages = bytes.Contains(c, []byte(`"image_url"`)) || bytes.Contains(c, []byte(`"input_image"`))
	}
	return pm
}

// ---------- Stage 2 Asynchronous LLM Summarization ----------

func (cm *ContextCompressor) stage2WorkerLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-cm.jobCh:
			if job != nil {
				cm.runStage2Job(ctx, job)
			}
		}
	}
}

func (cm *ContextCompressor) applyRedactionFilter(ctx context.Context, rawTargetURL string, body []byte, confidential bool) ([]byte, bool, error) {
	fn := cm.router.getWarmupFilter()
	if fn == nil {
		return body, false, nil
	}
	filterCtx := ctx
	if confidential {
		filterCtx = WithDataClass(ctx, "confidential")
	} else if dataClassFromContext(ctx) == "" {
		filterCtx = WithDataClass(ctx, "internal")
	}
	out, err := fn(filterCtx, cm.router.name, rawTargetURL, body)
	if err != nil {
		return nil, false, err
	}
	return out, !bytes.Equal(out, body), nil
}

func (cm *ContextCompressor) runStage2Job(ctx context.Context, job *stage2Job) {
	cm.stage2Active.Store(true)
	defer func() {
		cm.stage2Active.Store(false)
		cm.mu.Lock()
		delete(cm.inFlightSess, job.sessionRoot)
		cm.mu.Unlock()
	}()

	jobCtx := ctx
	if job.confidential || normalizeOriginTarget(job.originTarget) != "external" {
		cm.targetJobMu.Lock()
		abortCh := cm.nonExtAbortCh
		cm.targetJobMu.Unlock()
		var jobCancel context.CancelFunc
		jobCtx, jobCancel = context.WithCancel(ctx)
		defer jobCancel()
		go func() {
			select {
			case <-abortCh:
				jobCancel()
			case <-jobCtx.Done():
			}
		}()
	}

	if job.deferAfterLive {
		delay := 1500 * time.Millisecond
		if ns := cm.deferAfterLiveDelay.Load(); ns < 0 {
			delay = 0
		} else if ns > 0 {
			delay = time.Duration(ns)
		}
		if delay > 0 {
			t := time.NewTimer(delay)
			select {
			case <-jobCtx.Done():
				t.Stop()
				if ctx.Err() == nil {
					cm.purgeCheckpointOnBoundaryChange(job)
				} else {
					cm.resetPendingStage2Status(job.checkpointID)
				}
				return
			case <-t.C:
			}
		}
	}

	routerTarget, bErr := cm.checkJobTrustBoundary(job)
	if bErr != nil {
		cm.purgeCheckpointOnBoundaryChange(job)
		cm.router.log.Warn("stage2 skipped: trust boundary mismatch", "provider", cm.router.name, "id", shortID(job.checkpointID))
		return
	}
	if !cm.hasCheckpoint(job.checkpointID) {
		return
	}

	cfg := cm.getCfg()
	start := time.Now()
	cm.totalStage2Jobs.Add(1)

	upgradedOnStart := false
	cm.mu.Lock()
	if cp, ok := cm.checkpoints[job.checkpointID]; ok {
		if job.confidential && !cp.Confidential {
			cp.Confidential = true
			cm.markDirtyLocked()
			upgradedOnStart = true
		}
		cp.Stage2Status = "running"
	}
	cm.mu.Unlock()
	if upgradedOnStart {
		cm.saveToDisk()
	}

	var summaryText string
	var redactedChanged bool
	var usedBackend string
	var err error

	if sumURL := strings.TrimSpace(cfg.SummarizerURL); sumURL != "" {
		usedBackend = "external-summarizer"
		if job.confidential && (routerTarget != "local" || !config.IsLoopbackURL(sumURL)) {
			err = errors.New("confidential checkpoint cannot use external summarizer")
		} else {
			summaryText, redactedChanged, err = cm.callExternalSummarizer(jobCtx, cfg, job)
		}
	} else {
		backend := cm.waitForIdleBackend(jobCtx, cfg, job)
		if backend == nil {
			if _, bErr := cm.checkJobTrustBoundary(job); bErr != nil {
				cm.purgeCheckpointOnBoundaryChange(job)
				return
			}
			if !cm.hasCheckpoint(job.checkpointID) {
				return
			}
			if jobCtx.Err() != nil {
				cm.resetPendingStage2Status(job.checkpointID)
				return
			}
			err = fmt.Errorf("no idle eligible backend for stage2 (src_est=%d)", job.sourceEstTokens)
		} else {
			bCfg := backend.config()
			usedBackend = bCfg.ID
			summaryText, redactedChanged, err = cm.callClusterSummarizer(jobCtx, cfg, job, backend)
		}
	}

	durMs := time.Since(start).Milliseconds()
	if err != nil {
		if _, bErr := cm.checkJobTrustBoundary(job); bErr != nil {
			cm.purgeCheckpointOnBoundaryChange(job)
			return
		}
		if !cm.hasCheckpoint(job.checkpointID) {
			return
		}
		cm.totalStage2Fail.Add(1)
		upgradedOnErr := false
		cm.mu.Lock()
		if cp, ok := cm.checkpoints[job.checkpointID]; ok {
			if job.confidential && !cp.Confidential {
				cp.Confidential = true
				cm.markDirtyLocked()
				upgradedOnErr = true
			}
			cp.Stage2Status = "failed"
			cp.Stage2Err = err.Error()
			cp.Stage2Ms = durMs
			cp.Stage2FailedAt = time.Now()
			if !cp.Confidential {
				cm.markDirtyLocked()
			}
		}
		cm.mu.Unlock()
		if upgradedOnErr {
			cm.saveToDisk()
		}
		cm.router.log.Warn("context compress stage2 failed", "provider", cm.router.name, "id", shortID(job.checkpointID), "backend", usedBackend, "ms", durMs, "err", err)
		return
	}

	routerTarget, bErr = cm.checkJobTrustBoundary(job)
	if bErr != nil {
		cm.purgeCheckpointOnBoundaryChange(job)
		return
	}
	if !cm.hasCheckpoint(job.checkpointID) {
		return
	}

	summaryText = strings.TrimSpace(summaryText)
	if redactedChanged && routerTarget == "local" && normalizeOriginTarget(job.originTarget) == "local" {
		if restoreFn := cm.router.getWarmupRestore(); restoreFn != nil {
			summaryText = restoreFn(cm.router.name, summaryText)
		}
	}
	if utf8.RuneCountInString(summaryText) > maxSummaryRunes {
		rs := []rune(summaryText)
		summaryText = string(rs[:maxSummaryRunes]) + "\n[摘要已截断至最大配额]"
	}
	hardConstraints := extractHardConstraintsSection(summaryText, job.prevConstraints)
	summaryEst := estimateTextTokens(summaryText) + 64

	prewarmPrompt, prewarmCached := 0, 0
	if stage2PrewarmEnabled(cfg) && strings.TrimSpace(cfg.SummarizerURL) == "" {
		tmpCP := &ContextCheckpoint{
			ArchivedMsgCount: job.archivedCount,
			OriginalTokens:   job.originalTokens,
			SummaryText:      summaryText,
		}
		injected := buildStage2InjectedMessages(tmpCP, job.nextMsgRole)
		prewarmPrompt, prewarmCached = cm.prewarmStage2Checkpoint(jobCtx, job, injected)
	}

	shouldPersist := false
	cm.mu.Lock()
	if _, bErr := cm.checkJobTrustBoundary(job); bErr != nil {
		if cur, ok := cm.checkpoints[job.checkpointID]; ok {
			delete(cm.checkpoints, job.checkpointID)
			cm.rebuildSessionIndexLocked()
			if !cur.Confidential {
				cm.markDirtyLocked()
				shouldPersist = true
			}
		}
		cm.mu.Unlock()
		if shouldPersist {
			cm.saveToDisk()
		}
		return
	}
	if cp, ok := cm.checkpoints[job.checkpointID]; ok {
		wasNonConfidential := !cp.Confidential
		if job.confidential && !cp.Confidential {
			cp.Confidential = true
		}
		cp.Stage = 2
		cp.SummaryText = summaryText
		cp.HardConstraints = hardConstraints
		cp.SummaryTokens = summaryEst
		cp.Stage2Status = "ready"
		cp.Stage2Backend = usedBackend
		cp.Stage2Ms = durMs
		cp.Stage2Err = ""
		cp.PrewarmPrompt = prewarmPrompt
		cp.PrewarmCached = prewarmCached
		cp.Stage1Msgs = nil
		if !cp.Confidential || (wasNonConfidential && cp.Confidential) {
			cm.markDirtyLocked()
			shouldPersist = true
		}
	}
	cm.mu.Unlock()
	if shouldPersist {
		cm.saveToDisk()
	}

	cm.router.log.Info("context compress stage2 ready",
		"provider", cm.router.name, "id", shortID(job.checkpointID), "backend", usedBackend,
		"orig", job.originalTokens, "summary_est", summaryEst, "prewarm_cached", prewarmCached, "prewarm_prompt", prewarmPrompt, "ms", durMs)
}

func (cm *ContextCompressor) waitForIdleBackend(ctx context.Context, cfg ContextCompressConfig, job *stage2Job) *BackendState {
	need := job.sourceEstTokens + cfg.Stage2MaxSummaryTokens*2 + 2048
	deadline := time.Now().Add(60 * time.Second)
	poll := 2 * time.Second
	if ns := cm.idlePollInterval.Load(); ns > 0 {
		poll = time.Duration(ns)
	}
	for {
		if _, err := cm.checkJobTrustBoundary(job); err != nil {
			return nil
		}
		if !cm.hasCheckpoint(job.checkpointID) {
			return nil
		}
		for _, b := range cm.router.getBackends() {
			if !b.healthy.Load() {
				continue
			}
			if b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
				continue
			}
			bCfg := b.config()
			if job.confidential && !config.IsLoopbackURL(bCfg.URL) {
				continue
			}
			if need <= bCfg.MaxContextTokens {
				if _, err := cm.checkJobTrustBoundary(job); err != nil {
					return nil
				}
				return b
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}

func buildStage2Prompt(prevConstraints string) string {
	var sb strings.Builder
	sb.WriteString("【服务端上下文压缩指令】请将以上全部历史对话（不含本条指令）浓缩为一份高保真结构化 Checkpoint 摘要，供后续对话无缝衔接。\n")
	sb.WriteString("要求：不要调用任何工具；请简短思考（不超过300字）后直接输出结构化摘要，不要冗长推演；总篇幅控制在 2000~3500 字以内。\n\n")
	sb.WriteString("必须严格按以下 5 个分栏与权重输出：\n")
	sb.WriteString("## 1. 用户目标、硬约束与最新纠偏（高权 ~20% · 严禁泛化）\n")
	sb.WriteString("- 完整保留用户核心目标、明确禁令/红线（如禁止改动某文件/节点/配置）及用户中途纠偏后的最新口径。\n")
	if strings.TrimSpace(prevConstraints) != "" {
		sb.WriteString("- 【跨代继承硬约束】：除非上方最新对话中用户明确撤回或修改，以下既有红线必须原样保留进本节：\n")
		sb.WriteString(prevConstraints)
		sb.WriteString("\n")
	}
	sb.WriteString("\n## 2. 已落地的事实变更与环境状态（高权 ~25% · 保留精确字面量）\n")
	sb.WriteString("- 精确记录已生效的代码/配置改动、文件路径、函数/字段名、端口号、IP/节点名、已执行命令及当前环境状态。\n")
	sb.WriteString("\n## 3. 未完成待办、阻塞点与待确认项（高权 ~20%）\n")
	sb.WriteString("- 明确当前做到哪一步、尚未解决的 Bug/阻塞点、等待用户确认的决定；已完成项请移入第 2 节。\n")
	sb.WriteString("\n## 4. 关键结论与取舍理由（中权 ~20%）\n")
	sb.WriteString("- 只保留已定位的根因结论、关键实测指标/报错码、架构选型及取舍原因。\n")
	sb.WriteString("\n## 5. 核心实体索引（低权 ~15%）\n")
	sb.WriteString("- 列出涉及的关键文件/模块/接口路径 + 一句话职责，不抄录源码。\n\n")
	sb.WriteString("【明确忽略（严禁写入）】原始长日志/大段源码/HTML/JSON 原文、历史截图无关视觉细节、已推翻的死胡同试错过程（最多一句“已排除X”）、寒暄客套与内部思考草稿。\n")
	sb.WriteString("【防幻觉红线】所有路径、数值、配置项、报错码必须严格出自上方原文，严禁编造或脑补；若细节已被折叠且无法确认，请标注 [待重查]。")
	return sb.String()
}

func (cm *ContextCompressor) callClusterSummarizer(parentCtx context.Context, cfg ContextCompressConfig, job *stage2Job, backend *BackendState) (string, bool, error) {
	bCfg, bTarget := backend.snapshot()
	msgs := make([]json.RawMessage, 0, len(job.sysMsgs)+len(job.sourceHistoryMsgs)+1)
	msgs = append(msgs, job.sysMsgs...)
	msgs = append(msgs, job.sourceHistoryMsgs...)

	promptMsg, err := marshalNoEscapeHTML(map[string]string{
		"role":    "user",
		"content": buildStage2Prompt(job.prevConstraints),
	})
	if err != nil {
		return "", false, err
	}
	msgs = append(msgs, promptMsg)

	maxTok := cfg.Stage2MaxSummaryTokens * 2
	if maxTok < 8192 {
		maxTok = 8192
	}
	avail := bCfg.MaxContextTokens - job.sourceEstTokens - 2048
	if avail < 1024 {
		return "", false, fmt.Errorf("insufficient context headroom on %s (avail=%d)", bCfg.ID, avail)
	}
	if maxTok > avail {
		maxTok = avail
	}

	model := job.model
	if model == "" {
		model = "DeepSeek-V4.1-Flash-1017"
	}
	payload := map[string]any{
		"model":       model,
		"messages":    msgs,
		"max_tokens":  maxTok,
		"temperature": 0.2,
		"stream":      false,
	}
	if len(job.tools) > 0 && !bytes.Equal(bytes.TrimSpace(job.tools), []byte("null")) {
		payload["tools"] = job.tools
	}
	for k, v := range job.extraOpts {
		if k == "response_format" {
			continue
		}
		payload[k] = v
	}

	body, err := marshalNoEscapeHTML(payload)
	if err != nil {
		return "", false, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(cfg.Stage2TimeoutSec)*time.Second)
	defer cancel()

	body, redactedChanged, err := cm.applyRedactionFilter(ctx, bCfg.URL, body, job.confidential)
	if err != nil {
		return "", false, fmt.Errorf("blocked by redaction policy: %w", err)
	}

	targetURL := backendChatCompletionsURL(bTarget, bCfg.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return "", redactedChanged, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dsv41-smart-router-compressor/1.0")
	if apiKey := cm.router.getAPIKey(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	if _, err := cm.checkJobTrustBoundary(job); err != nil {
		return "", redactedChanged, err
	}
	if job.confidential && !config.IsLoopbackURL(bCfg.URL) {
		return "", redactedChanged, errors.New("confidential checkpoint cannot dispatch to non-loopback backend")
	}

	backend.activeReqs.Add(1)
	resp, err := cm.router.transport.RoundTrip(req)
	var respBody []byte
	var status int
	if err == nil {
		status = resp.StatusCode
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
		_ = resp.Body.Close()
	}
	backend.activeReqs.Add(-1)

	if err != nil {
		return "", redactedChanged, err
	}
	if status < 200 || status >= 300 {
		return "", redactedChanged, fmt.Errorf("HTTP %d: %s", status, truncateUTF8(string(respBody), 240))
	}
	sumText, err := extractAssistantContentFromResponse(respBody)
	return sumText, redactedChanged, err
}

func (cm *ContextCompressor) callExternalSummarizer(parentCtx context.Context, cfg ContextCompressConfig, job *stage2Job) (string, bool, error) {
	var transcript strings.Builder
	toolNameByID := make(map[string]string)
	for _, rm := range job.sourceHistoryMsgs {
		pm := parseHistoryMsg(rm)
		var tcs []parsedToolCall
		if pm.role == "assistant" {
			tcs = extractToolCalls(rm)
			for _, tc := range tcs {
				if tc.id != "" && tc.name != "" {
					toolNameByID[tc.id] = tc.name
				}
			}
		}
		if pm.text == "" && len(tcs) == 0 {
			continue
		}
		if pm.role == "tool" {
			tName := toolNameByID[pm.toolCallID]
			if tName != "" && pm.toolCallID != "" {
				fmt.Fprintf(&transcript, "[tool %s (%s)]\n", tName, pm.toolCallID)
			} else if pm.toolCallID != "" {
				fmt.Fprintf(&transcript, "[tool (%s)]\n", pm.toolCallID)
			} else {
				transcript.WriteString("[tool]\n")
			}
		} else {
			transcript.WriteString("[")
			transcript.WriteString(pm.role)
			transcript.WriteString("]\n")
		}
		if pm.text != "" {
			transcript.WriteString(pm.text)
			transcript.WriteString("\n")
		}
		for _, tc := range tcs {
			args := strings.TrimSpace(tc.arguments)
			if utf8.RuneCountInString(args) > 2000 {
				args = smartTruncateText(args, 2000, 0.65, 0.25, 4, "[工具参数过长已截断]")
			}
			if args == "" {
				args = "{}"
			}
			if tc.id != "" {
				fmt.Fprintf(&transcript, "tool_call %s(%s): %s\n", tc.name, tc.id, args)
			} else {
				fmt.Fprintf(&transcript, "tool_call %s: %s\n", tc.name, args)
			}
		}
		transcript.WriteString("\n")
	}

	model := cfg.SummarizerModel
	if model == "" {
		model = job.model
	}
	payload := map[string]any{
		"model": model,
		"messages": []map[string]string{
			{"role": "system", "content": "你是一个高精度的技术会话上下文压缩专家，严格忠实原文，绝不编造任何路径、参数或结论。"},
			{"role": "user", "content": transcript.String() + "\n---\n" + buildStage2Prompt(job.prevConstraints)},
		},
		"max_tokens":  cfg.Stage2MaxSummaryTokens,
		"temperature": 0.2,
		"stream":      false,
	}
	body, err := marshalNoEscapeHTML(payload)
	if err != nil {
		return "", false, err
	}

	ctx, cancel := context.WithTimeout(parentCtx, time.Duration(cfg.Stage2TimeoutSec)*time.Second)
	defer cancel()

	body, redactedChanged, err := cm.applyRedactionFilter(ctx, cfg.SummarizerURL, body, job.confidential)
	if err != nil {
		return "", false, fmt.Errorf("blocked by redaction policy: %w", err)
	}

	targetURL := strings.TrimRight(cfg.SummarizerURL, "/")
	if !strings.HasSuffix(targetURL, "/chat/completions") {
		targetURL = backendChatCompletionsURL(nil, targetURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return "", redactedChanged, err
	}
	req.Header.Set("Content-Type", "application/json")

	apiKey := ""
	if cfg.SummarizerKeyRef != "" {
		apiKey = cm.router.resolveSecret(cfg.SummarizerKeyRef)
	}
	if apiKey == "" {
		apiKey = cfg.SummarizerAPIKey
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	if _, err := cm.checkJobTrustBoundary(job); err != nil {
		return "", redactedChanged, err
	}

	resp, err := cm.router.transport.RoundTrip(req)
	if err != nil {
		return "", redactedChanged, err
	}
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", redactedChanged, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncateUTF8(string(respBody), 240))
	}
	sumText, err := extractAssistantContentFromResponse(respBody)
	return sumText, redactedChanged, err
}

func (cm *ContextCompressor) prewarmStage2Checkpoint(parentCtx context.Context, job *stage2Job, injected []json.RawMessage) (int, int) {
	if _, err := cm.checkJobTrustBoundary(job); err != nil || !cm.hasCheckpoint(job.checkpointID) {
		return 0, 0
	}
	estTok := estimateMessagesTokens(job.sysMsgs) + estimateToolsTokens(job.tools) + estimateMessagesTokens(injected) + 32
	var backend *BackendState
	for _, b := range cm.router.getBackends() {
		if !b.healthy.Load() || b.activeReqs.Load() != 0 || b.activeHeavy.Load() != 0 {
			continue
		}
		bCfg := b.config()
		if job.confidential && !config.IsLoopbackURL(bCfg.URL) {
			continue
		}
		if estTok <= bCfg.MaxSafeInput {
			backend = b
			break
		}
	}
	if backend == nil {
		return 0, 0
	}
	bCfg, bTarget := backend.snapshot()

	msgs := make([]json.RawMessage, 0, len(job.sysMsgs)+len(injected)+1)
	msgs = append(msgs, job.sysMsgs...)
	msgs = append(msgs, injected...)
	if len(injected) == 0 || extractMessageRole(injected[len(injected)-1]) != "user" {
		msgs = append(msgs, json.RawMessage(`{"role":"user","content":"."}`))
	}

	model := job.model
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
	if len(job.tools) > 0 && !bytes.Equal(bytes.TrimSpace(job.tools), []byte("null")) {
		payload["tools"] = job.tools
	}
	for k, v := range job.extraOpts {
		payload[k] = v
	}
	body, err := marshalNoEscapeHTML(payload)
	if err != nil {
		return 0, 0
	}

	ctx, cancel := context.WithTimeout(parentCtx, 90*time.Second)
	defer cancel()

	body, _, err = cm.applyRedactionFilter(ctx, bCfg.URL, body, job.confidential)
	if err != nil {
		return 0, 0
	}

	targetURL := backendChatCompletionsURL(bTarget, bCfg.URL)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, 0
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "dsv41-smart-router-prewarm/1.0")
	if apiKey := cm.router.getAPIKey(); apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	if _, err := cm.checkJobTrustBoundary(job); err != nil || !cm.hasCheckpoint(job.checkpointID) {
		return 0, 0
	}
	if job.confidential && !config.IsLoopbackURL(bCfg.URL) {
		return 0, 0
	}

	backend.activeReqs.Add(1)
	resp, err := cm.router.transport.RoundTrip(req)
	var respBody []byte
	var status int
	if err == nil {
		status = resp.StatusCode
		respBody, _ = io.ReadAll(io.LimitReader(resp.Body, 256*1024))
		_ = resp.Body.Close()
	}
	backend.activeReqs.Add(-1)
	if err != nil || status < 200 || status >= 300 {
		return 0, 0
	}
	return extractUsageFromTail(respBody)
}

func extractAssistantContentFromResponse(respBody []byte) (string, error) {
	var parsed struct {
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Message      struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", fmt.Errorf("failed parsing summarizer response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("empty choices in summarizer response")
	}
	fr := ""
	if parsed.Choices[0].FinishReason != nil {
		fr = strings.ToLower(strings.TrimSpace(*parsed.Choices[0].FinishReason))
	}
	switch fr {
	case "stop", "end_turn", "eos":
	case "length", "max_tokens", "model_context_window_exceeded":
		return "", fmt.Errorf("summarizer completion truncated by token limit (finish_reason=%q)", fr)
	default:
		return "", fmt.Errorf("incomplete summarizer completion (finish_reason=%q)", fr)
	}
	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	if len(content) < 60 {
		return "", fmt.Errorf("summary content too short (%d bytes)", len(content))
	}
	return content, nil
}

func extractHardConstraintsSection(summary, fallback string) string {
	idx1 := strings.Index(summary, "## 1.")
	if idx1 < 0 {
		idx1 = strings.Index(summary, "## 1 ")
	}
	if idx1 < 0 {
		return fallback
	}
	rest := summary[idx1:]
	idx2 := strings.Index(rest, "\n## 2")
	var sec string
	if idx2 > 0 {
		sec = strings.TrimSpace(rest[:idx2])
	} else {
		sec = strings.TrimSpace(rest)
	}
	if len(sec) > 2400 {
		sec = truncateUTF8(sec, 2400)
	}
	if len(sec) < 20 {
		return fallback
	}
	return sec
}

func buildStage2InjectedMessages(cp *ContextCheckpoint, nextRole string) []json.RawMessage {
	header := fmt.Sprintf("%s · 已归档前 %d 条历史消息 · 原始约 %d tokens]\n\n%s",
		summaryHeaderMarker, cp.ArchivedMsgCount, cp.OriginalTokens, cp.SummaryText)
	userMsg, _ := marshalNoEscapeHTML(map[string]string{
		"role":    "user",
		"content": header,
	})
	if nextRole == "assistant" {
		return []json.RawMessage{userMsg}
	}
	ackMsg, _ := marshalNoEscapeHTML(map[string]string{
		"role":    "assistant",
		"content": stage2AckContent,
	})
	return []json.RawMessage{userMsg, ackMsg}
}

// ---------- Canonical Body Hashing & Message Utilities ----------

func isBypassRequested(h http.Header) bool {
	if h == nil {
		return false
	}
	v := strings.ToLower(strings.TrimSpace(h.Get(compressHeaderBypass)))
	if v == "1" || v == "true" || v == "yes" {
		return true
	}
	c := strings.ToLower(strings.TrimSpace(h.Get(compressHeaderControl)))
	return c == "off" || c == "0" || c == "false" || c == "none"
}

func findLeadingSystemEnd(msgs []json.RawMessage) int {
	i := 0
	for i < len(msgs) {
		role := extractMessageRole(msgs[i])
		if role != "system" && role != "developer" {
			break
		}
		i++
	}
	return i
}

func extractMessageRole(raw json.RawMessage) string {
	var hdr struct {
		Role string `json:"role"`
	}
	if json.Unmarshal(raw, &hdr) == nil {
		return hdr.Role
	}
	return ""
}

func canonicalizeBodyMsg(raw json.RawMessage) []byte {
	if !bytes.Contains(raw, []byte(`"cache_control"`)) && !bytes.Contains(raw, []byte(`"reasoning`)) {
		return compactJSONBytes(raw)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return compactJSONBytes(raw)
	}
	delete(obj, "cache_control")
	delete(obj, "reasoning_content")
	delete(obj, "reasoning")
	if c, ok := obj["content"]; ok {
		tc := bytes.TrimSpace(c)
		if len(tc) > 0 && tc[0] == '[' && bytes.Contains(tc, []byte(`"cache_control"`)) {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(tc, &parts) == nil {
				for _, p := range parts {
					delete(p, "cache_control")
				}
				if enc, err := marshalNoEscapeHTML(parts); err == nil {
					obj["content"] = enc
				}
			}
		}
	}
	enc, err := marshalNoEscapeHTML(obj)
	if err != nil {
		return compactJSONBytes(raw)
	}
	return compactJSONBytes(enc)
}

func computeCheckpointScopeHash(originTarget string, confidential bool, callerScope string, sysMsgs []json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte("scope-v1|"))
	h.Write([]byte(normalizeOriginTarget(originTarget)))
	h.Write([]byte("|"))
	if confidential {
		h.Write([]byte("conf|"))
	} else {
		h.Write([]byte("pub|"))
	}
	h.Write([]byte(callerScope))
	h.Write([]byte("|"))
	for _, sm := range sysMsgs {
		h.Write(canonicalizeBodyMsg(sm))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func computeSingleBodyHash(prev string, raw json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(prev))
	h.Write([]byte("|"))
	h.Write(canonicalizeBodyMsg(raw))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func computeBodyHashes(seed string, msgs []json.RawMessage, sysEnd int) []string {
	hashes := make([]string, len(msgs))
	prev := seed
	for i := sysEnd; i < len(msgs); i++ {
		prev = computeSingleBodyHash(prev, msgs[i])
		hashes[i] = prev
	}
	return hashes
}

func rebuildBodyWithMessages(top map[string]json.RawMessage, sysMsgs, midMsgs, tailMsgs []json.RawMessage) ([]byte, error) {
	all := make([]json.RawMessage, 0, len(sysMsgs)+len(midMsgs)+len(tailMsgs))
	all = append(all, sysMsgs...)
	all = append(all, midMsgs...)
	all = append(all, tailMsgs...)

	encMsgs, err := marshalNoEscapeHTML(all)
	if err != nil {
		return nil, err
	}
	outMap := make(map[string]json.RawMessage, len(top))
	for k, v := range top {
		outMap[k] = v
	}
	outMap["messages"] = encMsgs
	return marshalNoEscapeHTML(outMap)
}

func estimateMessagesTokens(msgs []json.RawMessage) int {
	total := 0
	for _, m := range msgs {
		total += estimateMessageTokens(m)
	}
	return total
}

func estimateToolsTokens(tools json.RawMessage) int {
	t := bytes.TrimSpace(tools)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) || bytes.Equal(t, []byte("[]")) {
		return 0
	}
	return estimateTextTokens(string(t))
}

func extractModelName(top map[string]json.RawMessage) string {
	if rawM, ok := top["model"]; ok {
		var m string
		if json.Unmarshal(rawM, &m) == nil && m != "" {
			return m
		}
	}
	return "DeepSeek-V4.1-Flash-1017"
}

func extractPrefixExtraOpts(top map[string]json.RawMessage) map[string]json.RawMessage {
	opts := make(map[string]json.RawMessage)
	for _, k := range prefixOptionKeys {
		if v, ok := top[k]; ok {
			tv := bytes.TrimSpace(v)
			if len(tv) > 0 && !bytes.Equal(tv, []byte("null")) {
				opts[k] = cloneRawBytes(tv)
			}
		}
	}
	return opts
}

func cloneRawBytes(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	cp := make(json.RawMessage, len(b))
	copy(cp, b)
	return cp
}

func cloneRawMessages(in []json.RawMessage) []json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]json.RawMessage, len(in))
	for i, m := range in {
		out[i] = cloneRawBytes(m)
	}
	return out
}

func cloneExtraOpts(in map[string]json.RawMessage) map[string]json.RawMessage {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = cloneRawBytes(v)
	}
	return out
}

func shortID(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// ---------- Persistence, Eviction & Management Views ----------

func (cm *ContextCompressor) upgradeConfidential(id string) {
	if id == "" {
		return
	}
	upgraded := false
	cm.mu.Lock()
	if cp, ok := cm.checkpoints[id]; ok && !cp.Confidential {
		cp.Confidential = true
		cm.markDirtyLocked()
		upgraded = true
	}
	cm.mu.Unlock()
	if upgraded {
		cm.saveToDisk()
	}
}

func (cm *ContextCompressor) upsertCheckpoint(cp *ContextCheckpoint) {
	cfg := cm.getCfg()
	upgraded := false
	cm.mu.Lock()
	if prev := cm.checkpoints[cp.ID]; prev != nil {
		if prev.Confidential {
			cp.Confidential = true
		} else if cp.Confidential {
			upgraded = true
		}
	}
	if _, exists := cm.checkpoints[cp.ID]; !exists {
		cm.bySessionRoot[cp.SessionRoot] = append(cm.bySessionRoot[cp.SessionRoot], cp)
	} else {
		list := cm.bySessionRoot[cp.SessionRoot]
		replaced := false
		for i, item := range list {
			if item.ID == cp.ID {
				list[i] = cp
				replaced = true
				break
			}
		}
		if !replaced {
			cm.bySessionRoot[cp.SessionRoot] = append(list, cp)
		}
	}
	cm.checkpoints[cp.ID] = cp
	cm.trimLocked(cfg)
	if !cp.Confidential || upgraded {
		cm.markDirtyLocked()
	}
	cm.mu.Unlock()
	if upgraded {
		cm.saveToDisk()
	}
}

func (cm *ContextCompressor) touchCheckpoint(id string) {
	cm.mu.Lock()
	if cp, ok := cm.checkpoints[id]; ok {
		cp.Hits++
		cp.LastUsed = time.Now()
		if !cp.Confidential {
			cm.markDirtyLocked()
		}
	}
	cm.mu.Unlock()
}

func (cm *ContextCompressor) trimLocked(cfg ContextCompressConfig) {
	ttl := time.Duration(cfg.CheckpointTTLHours) * time.Hour
	now := time.Now()
	changedPersisted := false
	changed := false
	for id, cp := range cm.checkpoints {
		if ttl > 0 && now.Sub(cp.LastUsed) > ttl {
			delete(cm.checkpoints, id)
			changed = true
			if !cp.Confidential {
				changedPersisted = true
			}
		}
	}
	if cfg.MaxCheckpoints > 0 && len(cm.checkpoints) > cfg.MaxCheckpoints {
		all := make([]*ContextCheckpoint, 0, len(cm.checkpoints))
		for _, cp := range cm.checkpoints {
			all = append(all, cp)
		}
		sort.Slice(all, func(i, j int) bool {
			return all[i].LastUsed.After(all[j].LastUsed)
		})
		for _, cp := range all[cfg.MaxCheckpoints:] {
			delete(cm.checkpoints, cp.ID)
			changed = true
			if !cp.Confidential {
				changedPersisted = true
			}
		}
	}
	if changed {
		cm.rebuildSessionIndexLocked()
	}
	if changedPersisted {
		cm.markDirtyLocked()
	}
}

func (cm *ContextCompressor) rebuildSessionIndexLocked() {
	idx := make(map[string][]*ContextCheckpoint)
	for _, cp := range cm.checkpoints {
		idx[cp.SessionRoot] = append(idx[cp.SessionRoot], cp)
	}
	cm.bySessionRoot = idx
}

func (cm *ContextCompressor) loadFromDisk(path string) {
	if path == "" {
		return
	}
	cm.persistMu.Lock()
	defer cm.persistMu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			cm.router.log.Warn("context checkpoints load failed", "provider", cm.router.name, "path", path, "err", err)
		}
		return
	}
	var list []*ContextCheckpoint
	if err := json.Unmarshal(data, &list); err != nil {
		cm.router.log.Warn("context checkpoints parse failed", "provider", cm.router.name, "path", path, "err", err)
		return
	}
	routerTarget := cm.router.getTarget()
	dropped := 0
	cm.mu.Lock()
	for _, cp := range list {
		if cp == nil || cp.ID == "" {
			continue
		}
		if routerTarget != "local" && normalizeOriginTarget(cp.OriginTarget) != "external" {
			dropped++
			continue
		}
		if cp.Stage2Status == "running" || cp.Stage2Status == "pending" {
			cp.Stage2Status = "none"
		}
		cm.checkpoints[cp.ID] = cp
	}
	cm.rebuildSessionIndexLocked()
	if dropped > 0 {
		cm.markDirtyLocked()
	}
	count := len(cm.checkpoints)
	cm.mu.Unlock()
	if dropped > 0 {
		cm.saveToPathLocked(path)
	}
	cm.router.log.Info("loaded context checkpoints", "provider", cm.router.name, "count", count, "path", path)
}

func (cm *ContextCompressor) gcAndSave() {
	cfg := cm.getCfg()
	if cfg.PersistPath == "" {
		cm.mu.Lock()
		cm.trimLocked(cfg)
		cm.mu.Unlock()
		return
	}
	cm.persistMu.Lock()
	defer cm.persistMu.Unlock()
	cm.mu.Lock()
	cm.trimLocked(cfg)
	cm.mu.Unlock()
	cm.saveToPathLocked(cfg.PersistPath)
}

func (cm *ContextCompressor) saveToDisk() {
	cm.saveToPath(cm.getCfg().PersistPath)
}

func (cm *ContextCompressor) saveToPath(path string) {
	if path == "" {
		return
	}
	cm.persistMu.Lock()
	defer cm.persistMu.Unlock()
	cm.saveToPathLocked(path)
}

func (cm *ContextCompressor) saveToPathLocked(path string) {
	if path == "" {
		return
	}
	cfg := cm.getCfg()
	cm.mu.Lock()
	if !cm.dirty {
		cm.mu.Unlock()
		return
	}
	cm.trimLocked(cfg)
	seq := cm.dirtySeq
	list := make([]*ContextCheckpoint, 0, len(cm.checkpoints))
	for _, cp := range cm.checkpoints {
		if cp.Confidential {
			continue
		}
		copyCP := *cp
		copyCP.Stage1Msgs = cloneRawMessages(cp.Stage1Msgs)
		list = append(list, &copyCP)
	}
	cm.mu.Unlock()

	clearDirtyIfUnchanged := func() {
		cm.mu.Lock()
		if cm.dirtySeq == seq {
			cm.dirty = false
		}
		cm.mu.Unlock()
	}

	if len(list) == 0 {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			clearDirtyIfUnchanged()
			return
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		cm.router.log.Warn("context checkpoints mkdir failed", "provider", cm.router.name, "path", path, "err", err)
		return
	}
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		cm.router.log.Warn("context checkpoints marshal failed", "provider", cm.router.name, "err", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		cm.router.log.Warn("context checkpoints temp write failed", "provider", cm.router.name, "path", tmp, "err", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		cm.router.log.Warn("context checkpoints rename failed", "provider", cm.router.name, "path", path, "err", err)
		return
	}
	clearDirtyIfUnchanged()
}

func (cm *ContextCompressor) Summary() map[string]any {
	cfg := cm.getCfg()
	cm.mu.RLock()
	total := len(cm.checkpoints)
	stage1Cnt, stage2Cnt := 0, 0
	for _, cp := range cm.checkpoints {
		if cp.Stage == 1 {
			stage1Cnt++
		} else if cp.Stage == 2 {
			stage2Cnt++
		}
	}
	queued := len(cm.inFlightSess)
	cm.mu.RUnlock()
	return map[string]any{
		"enabled":              cfg.Enabled,
		"trigger_tokens":       cfg.TriggerTokens,
		"stage1_target_tokens": cfg.Stage1TargetTokens,
		"stage2_enabled":       stage2Enabled(cfg),
		"checkpoints_total":    total,
		"checkpoints_stage1":   stage1Cnt,
		"checkpoints_stage2":   stage2Cnt,
		"stage2_active":        cm.stage2Active.Load(),
		"stage2_in_flight":     queued,
		"total_stage1_hits":    cm.totalStage1Hits.Load(),
		"total_stage2_hits":    cm.totalStage2Hits.Load(),
		"total_stage2_jobs":    cm.totalStage2Jobs.Load(),
		"total_stage2_fails":   cm.totalStage2Fail.Load(),
		"total_saved_tokens":   cm.totalSavedToks.Load(),
	}
}

func (cm *ContextCompressor) List() []CheckpointView {
	cm.mu.RLock()
	items := make([]CheckpointView, 0, len(cm.checkpoints))
	for _, cp := range cm.checkpoints {
		prev := ""
		if cp.SummaryText != "" && !cp.Confidential {
			prev = strings.ReplaceAll(cp.SummaryText, "\n", " ")
			if len(prev) > 200 {
				prev = truncateUTF8(prev, 200) + "..."
			}
		}
		items = append(items, CheckpointView{
			ID:               cp.ID,
			SessionRoot:      cp.SessionRoot,
			OriginTarget:     cp.OriginTarget,
			Confidential:     cp.Confidential,
			Generation:       cp.Generation,
			Stage:            cp.Stage,
			CutMsgIdx:        cp.CutMsgIdx,
			ArchivedMsgCount: cp.ArchivedMsgCount,
			OriginalTokens:   cp.OriginalTokens,
			Stage1Tokens:     cp.Stage1Tokens,
			SummaryTokens:    cp.SummaryTokens,
			Stage2Status:     cp.Stage2Status,
			Stage2Backend:    cp.Stage2Backend,
			Stage2Ms:         cp.Stage2Ms,
			Stage2Err:        cp.Stage2Err,
			PrewarmPrompt:    cp.PrewarmPrompt,
			PrewarmCached:    cp.PrewarmCached,
			Hits:             cp.Hits,
			SummaryPreview:   prev,
			CreatedAt:        cp.CreatedAt.Format(time.RFC3339),
			LastUsed:         cp.LastUsed.Format(time.RFC3339),
		})
	}
	cm.mu.RUnlock()

	sort.Slice(items, func(i, j int) bool {
		return items[i].LastUsed > items[j].LastUsed
	})
	return items
}

func (cm *ContextCompressor) Delete(id string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", ErrCheckpointNotFound
	}
	cm.mu.Lock()
	deleted := ""
	wasConf := false
	for k, cp := range cm.checkpoints {
		if k == id || strings.HasPrefix(k, id) {
			wasConf = cp.Confidential
			delete(cm.checkpoints, k)
			deleted = k
			cm.rebuildSessionIndexLocked()
			if !wasConf {
				cm.markDirtyLocked()
			}
			break
		}
	}
	cm.mu.Unlock()
	if deleted == "" {
		return "", ErrCheckpointNotFound
	}
	if !wasConf {
		cm.saveToDisk()
	}
	return deleted, nil
}

func (cm *ContextCompressor) Clear() int {
	cm.mu.Lock()
	n := len(cm.checkpoints)
	hadNonConf := false
	for _, cp := range cm.checkpoints {
		if !cp.Confidential {
			hadNonConf = true
			break
		}
	}
	cm.checkpoints = make(map[string]*ContextCheckpoint)
	cm.bySessionRoot = make(map[string][]*ContextCheckpoint)
	if hadNonConf {
		cm.markDirtyLocked()
	}
	cm.mu.Unlock()
	if hadNonConf {
		cm.saveToDisk()
	}
	return n
}
