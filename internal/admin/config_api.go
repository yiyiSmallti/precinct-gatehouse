package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
)

var validHistoryRe = regexp.MustCompile(`^config-[0-9.-]+\.yaml$`)

func (s *Server) keyRefExists(ref string) bool {
	if ref == "" {
		return false
	}
	if name, ok := strings.CutPrefix(ref, "db:"); ok {
		v, err := getSetting(s.deps.DB, "provider_key:"+name)
		return err == nil && v != ""
	}
	return true
}

func (s *Server) providerHasKey(p config.Provider) bool {
	return s.keyRefExists(p.KeyRef)
}

func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request, _ string) {
	st := s.deps.Live.Status()
	raw := st.Snapshot.Cfg.RawCopy()
	for i := range raw.Providers {
		raw.Providers[i].HasKey = s.providerHasKey(raw.Providers[i])
		if raw.Providers[i].Tiered != nil && raw.Providers[i].Tiered.ContextCompress != nil {
			cc := raw.Providers[i].Tiered.ContextCompress
			cc.HasSummarizerKey = cc.SummarizerAPIKey != "" || s.keyRefExists(cc.SummarizerKeyRef)
			cc.SummarizerAPIKey = ""
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          st.Version,
		"loaded_at":        st.LoadedAt.UTC().Format(time.RFC3339),
		"path":             st.Path,
		"hash":             st.Hash,
		"source":           st.Source,
		"last_error":       st.LastError,
		"restart_required": st.RestartRequired,
		"raw":              raw,
	})
}

func applySectionToRaw(raw *config.Config, section string, data json.RawMessage) error {
	if len(data) == 0 {
		return errors.New("data is required")
	}
	switch section {
	case "providers":
		prevInlineKeys := map[string]string{}
		for _, prev := range raw.Providers {
			if prev.Tiered != nil && prev.Tiered.ContextCompress != nil && prev.Tiered.ContextCompress.SummarizerAPIKey != "" {
				prevInlineKeys[prev.Name] = prev.Tiered.ContextCompress.SummarizerAPIKey
			}
		}
		var provs []config.Provider
		if err := json.Unmarshal(data, &provs); err != nil {
			return err
		}
		for i := range provs {
			provs[i].HasKey = false
			if provs[i].Tiered != nil && provs[i].Tiered.ContextCompress != nil {
				cc := provs[i].Tiered.ContextCompress
				cc.HasSummarizerKey = false
				if cc.SummarizerAPIKey == "" && cc.SummarizerKeyRef == "" {
					if prevKey, ok := prevInlineKeys[provs[i].Name]; ok {
						cc.SummarizerAPIKey = prevKey
					}
				}
			}
		}
		raw.Providers = provs
	case "routing":
		var body struct {
			RoutingGroups map[string]*config.RoutingGroup `json:"routing_groups"`
			DefaultGroup  string                          `json:"default_group"`
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return err
		}
		raw.RoutingGroups = body.RoutingGroups
		raw.DefaultGroup = body.DefaultGroup
	case "aliases":
		var aliases []config.ModelAlias
		if err := json.Unmarshal(data, &aliases); err != nil {
			return err
		}
		raw.ModelAliases = aliases
	case "redaction":
		var red config.Redaction
		if err := json.Unmarshal(data, &red); err != nil {
			return err
		}
		raw.Redaction = red
	case "general":
		var gen struct {
			MaxBodyMB      *int                  `json:"max_body_mb"`
			RequestLogDays *int                  `json:"request_log_days"`
			Tokens         *config.TokenDefaults `json:"tokens"`
			// Startup fields: optional, applied on the next restart.
			Listen *string       `json:"listen"`
			Admin  *config.Admin `json:"admin"`
			Meter  *config.Meter `json:"meter"`
			Audit  *config.Audit `json:"audit"`
		}
		if err := json.Unmarshal(data, &gen); err != nil {
			return err
		}
		raw.MaxBodyMB = gen.MaxBodyMB
		raw.RequestLogDays = gen.RequestLogDays
		if gen.Tokens != nil {
			raw.Tokens = *gen.Tokens
		}
		if gen.Listen != nil {
			raw.Listen = strings.TrimSpace(*gen.Listen)
		}
		if gen.Admin != nil {
			raw.Admin = *gen.Admin
		}
		if gen.Meter != nil {
			// The console edits only endpoint and interval; keep collector
			// headers (often auth) when the submission does not carry them.
			if gen.Meter.OTLP != nil && gen.Meter.OTLP.Headers == nil && raw.Meter.OTLP != nil {
				gen.Meter.OTLP.Headers = raw.Meter.OTLP.Headers
			}
			raw.Meter = *gen.Meter
			if raw.Meter.OTLP != nil && strings.TrimSpace(raw.Meter.OTLP.Endpoint) == "" {
				raw.Meter.OTLP = nil
			}
		}
		if gen.Audit != nil {
			raw.Audit = *gen.Audit
		}
	default:
		return errors.New("unknown section: " + section)
	}
	return nil
}

func (s *Server) writeApplyOK(w http.ResponseWriter, res live.ApplyResult) {
	rr := res.RestartRequired
	if rr == nil {
		rr = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"version":          res.Version,
		"restart_required": rr,
		"warnings":         []string{},
	})
}

func (s *Server) writeApplyErr(w http.ResponseWriter, err error) {
	if errors.Is(err, live.ErrConflict) {
		st := s.deps.Live.Status()
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        "conflict",
			"current_hash": st.Hash,
		})
		return
	}
	var ve *config.ValidationError
	if errors.As(err, &ve) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": ve.Problems,
		})
		return
	}
	var be *live.BuildError
	if errors.As(err, &be) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":   "build_failed",
			"message": be.Error(),
		})
		return
	}
	// YAML syntax / type decode errors from config.Parse
	if strings.Contains(err.Error(), "yaml:") || strings.Contains(err.Error(), "invalid duration") {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}

func applySectionsMapToRaw(raw *config.Config, sections map[string]json.RawMessage) ([]string, error) {
	if len(sections) == 0 {
		return nil, errors.New("sections is required")
	}
	order := []string{"providers", "routing", "aliases", "redaction", "general"}
	seen := make(map[string]bool, len(order))
	applied := make([]string, 0, len(sections))
	for _, sec := range order {
		seen[sec] = true
		data, ok := sections[sec]
		if !ok {
			continue
		}
		if err := applySectionToRaw(raw, sec, data); err != nil {
			return nil, err
		}
		applied = append(applied, sec)
	}
	var extra []string
	for sec := range sections {
		if !seen[sec] {
			extra = append(extra, sec)
		}
	}
	sort.Strings(extra)
	for _, sec := range extra {
		if err := applySectionToRaw(raw, sec, sections[sec]); err != nil {
			return nil, err
		}
	}
	return applied, nil
}

func (s *Server) putConfigSections(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		BaseHash string                     `json:"base_hash"`
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	st := s.deps.Live.Status()
	if req.BaseHash != st.Hash {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        "conflict",
			"current_hash": st.Hash,
		})
		return
	}
	raw := st.Snapshot.Cfg.RawCopy()
	applied, err := applySectionsMapToRaw(raw, req.Sections)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	yamlBytes, err := config.MarshalRaw(raw)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := s.deps.Live.Apply(yamlBytes, "web:"+strings.Join(applied, ","), req.BaseHash)
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) putConfigSection(w http.ResponseWriter, r *http.Request, _ string) {
	section := r.PathValue("section")
	var req struct {
		BaseHash string          `json:"base_hash"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	st := s.deps.Live.Status()
	if req.BaseHash != st.Hash {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":        "conflict",
			"current_hash": st.Hash,
		})
		return
	}
	raw := st.Snapshot.Cfg.RawCopy()
	if err := applySectionToRaw(raw, section, req.Data); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	yamlBytes, err := config.MarshalRaw(raw)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := s.deps.Live.Apply(yamlBytes, "web:"+section, req.BaseHash)
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) getConfigYAML(w http.ResponseWriter, _ *http.Request, _ string) {
	st := s.deps.Live.Status()
	writeJSON(w, http.StatusOK, map[string]any{
		"yaml": string(st.Snapshot.Cfg.Source()),
		"hash": st.Hash,
	})
}

func (s *Server) putConfigYAML(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		YAML     string `json:"yaml"`
		BaseHash string `json:"base_hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	res, err := s.deps.Live.Apply([]byte(req.YAML), "yaml", req.BaseHash)
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) validateConfig(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		YAML     *string                    `json:"yaml"`
		Section  string                     `json:"section"`
		Data     json.RawMessage            `json:"data"`
		Sections map[string]json.RawMessage `json:"sections"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{err.Error()},
		})
		return
	}
	st := s.deps.Live.Status()
	var yamlBytes []byte
	if req.YAML != nil {
		yamlBytes = []byte(*req.YAML)
	} else if len(req.Sections) > 0 {
		raw := st.Snapshot.Cfg.RawCopy()
		if _, err := applySectionsMapToRaw(raw, req.Sections); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":    "invalid_config",
				"problems": []string{err.Error()},
			})
			return
		}
		var err error
		yamlBytes, err = config.MarshalRaw(raw)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else if req.Section != "" {
		raw := st.Snapshot.Cfg.RawCopy()
		if err := applySectionToRaw(raw, req.Section, req.Data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error":    "invalid_config",
				"problems": []string{err.Error()},
			})
			return
		}
		var err error
		yamlBytes, err = config.MarshalRaw(raw)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":    "invalid_config",
			"problems": []string{"yaml or section+data is required"},
		})
		return
	}
	newCfg, err := s.deps.Live.Preview(yamlBytes)
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	rr := config.RestartRequiredDiff(st.Snapshot.Cfg, newCfg)
	if rr == nil {
		rr = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"version":          st.Version,
		"restart_required": rr,
		"warnings":         []string{},
	})
}

func (s *Server) reloadConfig(w http.ResponseWriter, _ *http.Request, _ string) {
	res, _, err := s.deps.Live.ReloadFromDisk("web")
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) listConfigHistory(w http.ResponseWriter, _ *http.Request, _ string) {
	list, err := s.deps.Live.HistoryList()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"history": list})
}

func (s *Server) getConfigHistory(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("name")
	if !validHistoryRe.MatchString(name) {
		writeJSONError(w, http.StatusBadRequest, "invalid history filename")
		return
	}
	b, err := s.deps.Live.HistoryRead(name)
	if errors.Is(err, os.ErrNotExist) {
		writeJSONError(w, http.StatusNotFound, "history entry not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": string(b)})
}

func (s *Server) rollbackConfig(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if !validHistoryRe.MatchString(req.Name) {
		writeJSONError(w, http.StatusBadRequest, "invalid history filename")
		return
	}
	res, err := s.deps.Live.Rollback(req.Name)
	if errors.Is(err, os.ErrNotExist) {
		writeJSONError(w, http.StatusNotFound, "history entry not found")
		return
	}
	if err != nil {
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) testProvider(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	snap := s.deps.Live.Current()
	prov, model, ok := snap.Reg.Lookup(req.Provider + "/" + req.Model)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"ok":    false,
			"error": "provider not found",
		})
		return
	}
	if model == "" && len(prov.Models) > 0 {
		model = prov.Models[0]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	payload := oai.Encode(map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
		"max_tokens": 8,
		"stream":     false,
	})
	start := time.Now()
	resp, err := prov.Chat(ctx, payload)
	lat := time.Since(start).Milliseconds()
	if err != nil {
		status := 0
		msg := err.Error()
		var ue *provider.UpstreamError
		if errors.As(err, &ue) {
			status = ue.Status
			if len(ue.Body) > 0 {
				if eo, derr := oai.Decode(ue.Body); derr == nil {
					if eobj, ok := eo["error"].(map[string]any); ok {
						if m := oai.Str(eobj, "message"); m != "" {
							msg = m
						}
					}
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":         false,
			"status":     status,
			"latency_ms": lat,
			"error":      msg,
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	sample := ""
	if o, derr := oai.Decode(body); derr == nil {
		if ch, _ := o["choices"].([]any); len(ch) > 0 {
			if c0, ok := ch[0].(map[string]any); ok {
				if msgObj, ok := c0["message"].(map[string]any); ok {
					sample = oai.Str(msgObj, "content")
					if sample == "" {
						sample = oai.Str(msgObj, "reasoning_content")
					}
				}
			}
		}
	}
	runes := []rune(sample)
	if len(runes) > 120 {
		sample = string(runes[:120])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"status":     resp.StatusCode,
		"latency_ms": lat,
		"sample":     sample,
	})
}

var errProviderNotFound = errors.New("provider not found")

func (s *Server) updateProviderKey(w http.ResponseWriter, providerName, baseHash string, applyChange func(raw *config.Config, idx int) (rollback func(), err error)) {
	res, err := s.deps.Live.ApplySerialized("web:providers", baseHash, func(prev *live.Snapshot) ([]byte, func(), error) {
		raw := prev.Cfg.RawCopy()
		idx := -1
		for i := range raw.Providers {
			if raw.Providers[i].Name == providerName {
				idx = i
				break
			}
		}
		if idx < 0 {
			return nil, nil, errProviderNotFound
		}
		rollback, err := applyChange(raw, idx)
		if err != nil {
			return nil, nil, err
		}
		yamlBytes, err := config.MarshalRaw(raw)
		if err != nil {
			if rollback != nil {
				rollback()
			}
			return nil, nil, err
		}
		return yamlBytes, rollback, nil
	})
	if err != nil {
		if errors.Is(err, errProviderNotFound) {
			writeJSONError(w, http.StatusNotFound, "provider not found")
			return
		}
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) putProviderKey(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("name")
	var req struct {
		Key      string `json:"key"`
		BaseHash string `json:"base_hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		writeJSONError(w, http.StatusBadRequest, "key is required")
		return
	}
	s.updateProviderKey(w, name, req.BaseHash, func(raw *config.Config, idx int) (func(), error) {
		prevVal, prevErr := getSetting(s.deps.DB, "provider_key:"+name)
		if err := setSetting(s.deps.DB, "provider_key:"+name, req.Key); err != nil {
			return nil, err
		}
		raw.Providers[idx].KeyRef = "db:" + name
		rollback := func() {
			if prevErr == nil {
				_ = setSetting(s.deps.DB, "provider_key:"+name, prevVal)
			} else {
				_, _ = s.deps.DB.Exec(`DELETE FROM settings WHERE k=?`, "provider_key:"+name)
			}
		}
		return rollback, nil
	})
}

func (s *Server) deleteProviderKey(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("name")
	baseHash := strings.TrimSpace(r.URL.Query().Get("base_hash"))
	if baseHash == "" && r.Body != nil {
		var req struct {
			BaseHash string `json:"base_hash"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err == nil {
			baseHash = strings.TrimSpace(req.BaseHash)
		} else if !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	s.updateProviderKey(w, name, baseHash, func(raw *config.Config, idx int) (func(), error) {
		prevVal, prevErr := getSetting(s.deps.DB, "provider_key:"+name)
		if _, err := s.deps.DB.Exec(`DELETE FROM settings WHERE k=?`, "provider_key:"+name); err != nil {
			return nil, err
		}
		raw.Providers[idx].KeyRef = ""
		rollback := func() {
			if prevErr == nil {
				_ = setSetting(s.deps.DB, "provider_key:"+name, prevVal)
			}
		}
		return rollback, nil
	})
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, user string) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(req.New) < 8 {
		writeJSONError(w, http.StatusBadRequest, "new password must be at least 8 characters")
		return
	}
	storedPass, err := getSetting(s.deps.DB, "admin_pass")
	if err != nil || !VerifyPassword(storedPass, req.Old) {
		writeJSONError(w, http.StatusBadRequest, "current password is incorrect")
		return
	}
	storedUser, err := getSetting(s.deps.DB, "admin_user")
	if err != nil || storedUser == "" {
		storedUser = user
	}
	if err := SetPassword(s.deps.DB, storedUser, req.New); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.auth.clearSessions()
	newPassHash, err := getSetting(s.deps.DB, "admin_pass")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ttl := s.sessionTTL()
	tok, err := s.auth.createSession(storedUser, newPassHash, ttl, time.Now())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSessionCookie(w, r, tok, ttl)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) flow(w http.ResponseWriter, r *http.Request, _ string) {
	rng := r.URL.Query().Get("range")
	dur := time.Hour
	if rng == "24h" {
		dur = 24 * time.Hour
	} else {
		rng = "1h"
	}
	cutoff := time.Now().Add(-dur).Unix()
	rows, err := s.deps.DB.Query(`
		SELECT status, error_code, COALESCE(NULLIF(model,''), NULLIF(requested_model,''), ''), ttft_ms
		FROM request_log WHERE ts >= ?`, cutoff)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var total, authRej, policyRej, rateLim, redBlock, upFail, otherFail, succeeded int64
	var ttftSum, ttftCnt int64
	errCounts := map[string]map[string]int64{
		"auth":      {},
		"policy":    {},
		"rate":      {},
		"redaction": {},
		"upstream":  {},
	}
	type modelAcc struct {
		succeeded int64
		total     int64
	}
	models := map[string]*modelAcc{}

	for rows.Next() {
		var status int
		var code, model string
		var ttft int64
		if err := rows.Scan(&status, &code, &model, &ttft); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		total++
		if model != "" {
			ma := models[model]
			if ma == nil {
				ma = &modelAcc{}
				models[model] = ma
			}
			ma.total++
			if status >= 200 && status < 400 {
				ma.succeeded++
			}
		}
		switch {
		case status >= 200 && status < 400:
			succeeded++
			if ttft > 0 {
				ttftSum += ttft
				ttftCnt++
			}
		case status == 401:
			authRej++
			if code != "" {
				errCounts["auth"][code]++
			}
		case status == 403:
			policyRej++
			if code != "" {
				errCounts["policy"][code]++
			}
		case status == 429:
			rateLim++
			if code != "" {
				errCounts["rate"][code]++
			}
		case code == "redaction_blocked" || code == "redaction_unavailable":
			redBlock++
			errCounts["redaction"][code]++
		case status >= 500 && (strings.HasPrefix(code, "upstream_") || code == "local_targets_unavailable" || code == "no_allowed_target"):
			upFail++
			if code != "" {
				errCounts["upstream"][code]++
			}
		default:
			otherFail++
		}
	}
	if err := rows.Err(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	type codeCount struct {
		Code  string `json:"code"`
		Count int64  `json:"count"`
	}
	topErrs := map[string][]codeCount{}
	for stage, m := range errCounts {
		var list []codeCount
		for c, n := range m {
			list = append(list, codeCount{Code: c, Count: n})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].Count != list[j].Count {
				return list[i].Count > list[j].Count
			}
			return list[i].Code < list[j].Code
		})
		if len(list) > 3 {
			list = list[:3]
		}
		if list == nil {
			list = []codeCount{}
		}
		topErrs[stage] = list
	}

	type modelFlow struct {
		Model     string `json:"model"`
		Succeeded int64  `json:"succeeded"`
		Total     int64  `json:"total"`
	}
	byModel := make([]modelFlow, 0, len(models))
	for m, acc := range models {
		byModel = append(byModel, modelFlow{Model: m, Succeeded: acc.succeeded, Total: acc.total})
	}
	sort.Slice(byModel, func(i, j int) bool {
		if byModel[i].Succeeded != byModel[j].Succeeded {
			return byModel[i].Succeeded > byModel[j].Succeeded
		}
		return byModel[i].Model < byModel[j].Model
	})

	var avgTTFT float64
	if ttftCnt > 0 {
		avgTTFT = float64(ttftSum) / float64(ttftCnt)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"range":             rng,
		"total":             total,
		"auth_rejected":     authRej,
		"policy_rejected":   policyRej,
		"rate_limited":      rateLim,
		"redaction_blocked": redBlock,
		"upstream_failed":   upFail,
		"other_failed":      otherFail,
		"succeeded":         succeeded,
		"top_errors":        topErrs,
		"by_model":          byModel,
		"avg_ttft_ms":       avgTTFT,
	})
}
