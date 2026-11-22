package admin

import (
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/provider"
)

//go:embed ui/*
var uiFiles embed.FS

// Deps holds the dependencies required by the admin server.
type Deps struct {
	DB         *sql.DB
	Keys       *keys.Store
	Limiter    *limit.Limiter
	Reg        *provider.Registry
	Cfg        *config.Config
	SessionTTL time.Duration
}

// Server serves the /admin/ Web UI and /admin/api/* JSON API.
type Server struct {
	deps Deps
	auth *authStore
	ui   fs.FS
	mux  *http.ServeMux
}

// New constructs an admin HTTP handler.
func New(d Deps) *Server {
	sub, err := fs.Sub(uiFiles, "ui")
	if err != nil {
		panic(err)
	}
	s := &Server{
		deps: d,
		auth: newAuthStore(d.SessionTTL),
		ui:   sub,
		mux:  http.NewServeMux(),
	}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/admin/", http.StatusFound)
	})
	s.mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("GET /admin/", s.serveUI("index.html", "text/html; charset=utf-8"))
	s.mux.HandleFunc("GET /admin/app.js", s.serveUI("app.js", "application/javascript; charset=utf-8"))
	s.mux.HandleFunc("GET /admin/app.css", s.serveUI("app.css", "text/css; charset=utf-8"))

	s.mux.HandleFunc("POST /admin/api/login", s.login)
	s.mux.HandleFunc("POST /admin/api/logout", s.guard(s.logout))
	s.mux.HandleFunc("GET /admin/api/me", s.guard(s.me))
	s.mux.HandleFunc("GET /admin/api/overview", s.guard(s.overview))
	s.mux.HandleFunc("GET /admin/api/keys", s.guard(s.listKeys))
	s.mux.HandleFunc("POST /admin/api/keys", s.guard(s.createKey))
	s.mux.HandleFunc("PATCH /admin/api/keys/{id}", s.guard(s.updateKey))
	s.mux.HandleFunc("POST /admin/api/keys/{id}/reset", s.guard(s.resetKey))
	s.mux.HandleFunc("DELETE /admin/api/keys/{id}", s.guard(s.deleteKey))
	s.mux.HandleFunc("GET /admin/api/logs", s.guard(s.logs))
	s.mux.HandleFunc("GET /admin/api/usage", s.guard(s.usage))
	s.mux.HandleFunc("GET /admin/api/channels", s.guard(s.channels))
}

func (s *Server) serveUI(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name == "index.html" && r.URL.Path != "/admin/" {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(s.ui, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(b)
	}
}

func (s *Server) guard(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, ok := s.auth.verifySession(c.Value, time.Now())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("X-Gatehouse-Admin") != "1" {
				writeJSONError(w, http.StatusForbidden, "missing X-Gatehouse-Admin header")
				return
			}
		}
		next(w, r, user)
	}
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	now := time.Now()
	if s.auth.isLocked(ip, now) {
		writeJSONError(w, http.StatusTooManyRequests, "too many failed login attempts")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	storedUser, errU := getSetting(s.deps.DB, "admin_user")
	storedPass, errP := getSetting(s.deps.DB, "admin_pass")
	if errU != nil || errP != nil || storedPass == "" {
		writeJSONError(w, http.StatusUnauthorized, "admin password not configured")
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(req.Username), []byte(storedUser)) == 1
	passOK := VerifyPassword(storedPass, req.Password)
	if !userOK || !passOK {
		s.auth.recordFail(ip, now)
		writeJSONError(w, http.StatusUnauthorized, "invalid username or password")
		return
	}
	s.auth.clearFails(ip)
	tok, err := s.auth.createSession(storedUser, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(s.auth.ttl.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"username": storedUser})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ string) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.auth.deleteSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    "",
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, user string) {
	writeJSON(w, http.StatusOK, map[string]any{"username": user})
}

func (s *Server) overview(w http.ResponseWriter, _ *http.Request, _ string) {
	now := time.Now()
	today := now.Local().Format("2006-01-02")

	var todaySummary struct {
		Requests         int64   `json:"requests"`
		Errors           int64   `json:"errors"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		CachedTokens     int64   `json:"cached_tokens"`
		AvgTTFTMS        float64 `json:"avg_ttft_ms"`
	}
	err := s.deps.DB.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(cached_tokens), 0),
		       COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END), 0)
		FROM request_log WHERE day=?`, today).Scan(
		&todaySummary.Requests, &todaySummary.Errors,
		&todaySummary.PromptTokens, &todaySummary.CompletionTokens,
		&todaySummary.CachedTokens, &todaySummary.AvgTTFTMS,
	)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 24 hourly buckets ending at current local hour.
	curHour := now.Local().Truncate(time.Hour)
	startHour := curHour.Add(-23 * time.Hour)
	type hourBucket struct {
		Hour     string `json:"hour"`
		TS       int64  `json:"ts"`
		Requests int64  `json:"requests"`
		Errors   int64  `json:"errors"`
	}
	hourly := make([]hourBucket, 24)
	for i := 0; i < 24; i++ {
		t := startHour.Add(time.Duration(i) * time.Hour)
		hourly[i] = hourBucket{Hour: t.Format("15:04"), TS: t.Unix()}
	}
	rows, err := s.deps.DB.Query(`SELECT ts, status FROM request_log WHERE ts >= ?`, startHour.Unix())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	startUnix := startHour.Unix()
	for rows.Next() {
		var ts int64
		var status int
		if err := rows.Scan(&ts, &status); err != nil {
			rows.Close()
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		idx := int((ts - startUnix) / 3600)
		if idx >= 0 && idx < 24 {
			hourly[idx].Requests++
			if status >= 400 {
				hourly[idx].Errors++
			}
		}
	}
	rows.Close()

	type modelStat struct {
		Model            string `json:"model"`
		Requests         int64  `json:"requests"`
		Errors           int64  `json:"errors"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
		CachedTokens     int64  `json:"cached_tokens"`
	}
	byModel := []modelStat{}
	mrows, err := s.deps.DB.Query(`
		SELECT COALESCE(NULLIF(model,''), NULLIF(requested_model,''), '(unknown)') AS m,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(prompt_tokens), 0),
		       COALESCE(SUM(completion_tokens), 0),
		       COALESCE(SUM(cached_tokens), 0)
		FROM request_log WHERE day=?
		GROUP BY m ORDER BY COUNT(*) DESC`, today)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for mrows.Next() {
		var ms modelStat
		if err := mrows.Scan(&ms.Model, &ms.Requests, &ms.Errors, &ms.PromptTokens, &ms.CompletionTokens, &ms.CachedTokens); err != nil {
			mrows.Close()
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		byModel = append(byModel, ms)
	}
	mrows.Close()

	allKeys, err := s.deps.Keys.List()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type keyLive struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Inflight  int    `json:"inflight"`
		RPMUsed   int    `json:"rpm_used"`
		TodayUsed int    `json:"today_used"`
	}
	inflightKeys := []keyLive{}
	totalInflight := 0
	for _, k := range allKeys {
		snap := s.deps.Limiter.Snapshot(k.ID, now)
		totalInflight += snap.Inflight
		inflightKeys = append(inflightKeys, keyLive{
			ID:        k.ID,
			Name:      k.Name,
			Inflight:  snap.Inflight,
			RPMUsed:   snap.RPMUsed,
			TodayUsed: snap.TodayUsed,
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"today":          todaySummary,
		"hourly":         hourly,
		"by_model":       byModel,
		"inflight_total": totalInflight,
		"keys":           inflightKeys,
	})
}

type keyView struct {
	keys.Key
	State     string `json:"state"`
	Inflight  int    `json:"inflight"`
	RPMUsed   int    `json:"rpm_used"`
	TodayUsed int    `json:"today_used"`
}

func (s *Server) makeKeyView(k keys.Key, now time.Time) keyView {
	state := "enabled"
	if !k.Enabled {
		state = "disabled"
	} else if k.ExpiresAt > 0 && now.Unix() >= k.ExpiresAt {
		state = "expired"
	}
	snap := s.deps.Limiter.Snapshot(k.ID, now)
	return keyView{
		Key:       k,
		State:     state,
		Inflight:  snap.Inflight,
		RPMUsed:   snap.RPMUsed,
		TodayUsed: snap.TodayUsed,
	}
}

func (s *Server) listKeys(w http.ResponseWriter, _ *http.Request, _ string) {
	list, err := s.deps.Keys.List()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	now := time.Now()
	out := make([]keyView, 0, len(list))
	for _, k := range list {
		out = append(out, s.makeKeyView(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Name        string   `json:"name"`
		Models      []string `json:"models"`
		Groups      []string `json:"groups"`
		RPM         int      `json:"rpm"`
		RPD         int      `json:"rpd"`
		Concurrency int      `json:"concurrency"`
		ExpiresAt   int64    `json:"expires_at"`
		Note        string   `json:"note"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	plain, k, err := s.deps.Keys.Create(keys.CreateOpts{
		Name:        req.Name,
		ExpiresAt:   req.ExpiresAt,
		Models:      req.Models,
		Groups:      req.Groups,
		RPM:         req.RPM,
		RPD:         req.RPD,
		Concurrency: req.Concurrency,
		Note:        req.Note,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":     plain,
		"api_key": s.makeKeyView(k, time.Now()),
	})
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	var req struct {
		Name        *string   `json:"name"`
		Models      *[]string `json:"models"`
		Groups      *[]string `json:"groups"`
		RPM         *int      `json:"rpm"`
		RPD         *int      `json:"rpd"`
		Concurrency *int      `json:"concurrency"`
		ExpiresAt   *int64    `json:"expires_at"`
		Note        *string   `json:"note"`
		Enabled     *bool     `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	k, err := s.deps.Keys.Update(id, keys.Patch{
		Name:        req.Name,
		Models:      req.Models,
		Groups:      req.Groups,
		RPM:         req.RPM,
		RPD:         req.RPD,
		Concurrency: req.Concurrency,
		ExpiresAt:   req.ExpiresAt,
		Note:        req.Note,
		Enabled:     req.Enabled,
	})
	if errors.Is(err, keys.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_key": s.makeKeyView(k, time.Now())})
}

func (s *Server) resetKey(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	plain, k, err := s.deps.Keys.Reset(id)
	if errors.Is(err, keys.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key":     plain,
		"api_key": s.makeKeyView(k, time.Now()),
	})
}

func (s *Server) deleteKey(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	err := s.deps.Keys.Delete(id)
	if errors.Is(err, keys.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	limit := 50
	if ls := q.Get("limit"); ls != "" {
		if n, err := strconv.Atoi(ls); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		limit = 1
	} else if limit > 200 {
		limit = 200
	}

	var where []string
	var args []any
	if k := strings.TrimSpace(q.Get("key")); k != "" {
		where = append(where, "(key_id=? OR key_name LIKE ?)")
		args = append(args, k, "%"+k+"%")
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		where = append(where, "(model LIKE ? OR requested_model LIKE ?)")
		args = append(args, "%"+m+"%", "%"+m+"%")
	}
	switch q.Get("status") {
	case "ok":
		where = append(where, "status >= 200 AND status < 400")
	case "error":
		where = append(where, "status >= 400")
	}
	if p := strings.TrimSpace(q.Get("protocol")); p != "" {
		where = append(where, "protocol = ?")
		args = append(args, p)
	}
	if b := strings.TrimSpace(q.Get("before_id")); b != "" {
		if bid, err := strconv.ParseInt(b, 10, 64); err == nil && bid > 0 {
			where = append(where, "id < ?")
			args = append(args, bid)
		}
	}
	sqlStr := `SELECT id,request_id,ts,day,key_id,key_name,protocol,client_ip,requested_model,model,
		status,error_code,stream,prompt_tokens,completion_tokens,cached_tokens,latency_ms,ttft_ms,admitted
		FROM request_log`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)

	rows, err := s.deps.DB.Query(sqlStr, args...)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type logRow struct {
		ID               int64  `json:"id"`
		RequestID        string `json:"request_id"`
		TS               int64  `json:"ts"`
		Day              string `json:"day"`
		KeyID            string `json:"key_id"`
		KeyName          string `json:"key_name"`
		Protocol         string `json:"protocol"`
		ClientIP         string `json:"client_ip"`
		RequestedModel   string `json:"requested_model"`
		Model            string `json:"model"`
		Status           int    `json:"status"`
		ErrorCode        string `json:"error_code"`
		Stream           bool   `json:"stream"`
		PromptTokens     int    `json:"prompt_tokens"`
		CompletionTokens int    `json:"completion_tokens"`
		CachedTokens     int    `json:"cached_tokens"`
		LatencyMS        int64  `json:"latency_ms"`
		TTFTMS           int64  `json:"ttft_ms"`
		Admitted         bool   `json:"admitted"`
	}
	out := []logRow{}
	hasMore := false
	for rows.Next() {
		var row logRow
		var streamInt, admInt int
		if err := rows.Scan(&row.ID, &row.RequestID, &row.TS, &row.Day, &row.KeyID, &row.KeyName,
			&row.Protocol, &row.ClientIP, &row.RequestedModel, &row.Model,
			&row.Status, &row.ErrorCode, &streamInt, &row.PromptTokens, &row.CompletionTokens,
			&row.CachedTokens, &row.LatencyMS, &row.TTFTMS, &admInt); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		row.Stream = streamInt != 0
		row.Admitted = admInt != 0
		if len(out) == limit {
			hasMore = true
			break
		}
		out = append(out, row)
	}
	var nextBefore int64
	if len(out) > 0 && hasMore {
		nextBefore = out[len(out)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs":           out,
		"has_more":       hasMore,
		"next_before_id": nextBefore,
	})
}

func (s *Server) usage(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	since := strings.TrimSpace(q.Get("since"))
	by := q.Get("by")
	if by == "" {
		by = "day"
	}
	var query string
	switch by {
	case "day":
		query = `SELECT day, '' AS sub, COUNT(*), COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(AVG(latency_ms),0), COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)
			FROM request_log WHERE day >= ? GROUP BY day ORDER BY day DESC`
	case "key":
		query = `SELECT COALESCE(NULLIF(key_name,''), NULLIF(key_id,''), '(anonymous)'), key_id,
			COUNT(*), COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(AVG(latency_ms),0), COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)
			FROM request_log WHERE day >= ? GROUP BY key_id, key_name ORDER BY COUNT(*) DESC`
	case "model":
		query = `SELECT COALESCE(NULLIF(model,''), NULLIF(requested_model,''), '(unknown)') AS m, '' AS sub,
			COUNT(*), COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(AVG(latency_ms),0), COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)
			FROM request_log WHERE day >= ? GROUP BY m ORDER BY COUNT(*) DESC`
	default:
		writeJSONError(w, http.StatusBadRequest, "by must be day, key or model")
		return
	}

	rows, err := s.deps.DB.Query(query, since)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type usageRow struct {
		Label            string  `json:"label"`
		KeyID            string  `json:"key_id,omitempty"`
		Requests         int64   `json:"requests"`
		Errors           int64   `json:"errors"`
		PromptTokens     int64   `json:"prompt_tokens"`
		CompletionTokens int64   `json:"completion_tokens"`
		CachedTokens     int64   `json:"cached_tokens"`
		AvgLatencyMS     float64 `json:"avg_latency_ms"`
		AvgTTFTMS        float64 `json:"avg_ttft_ms"`
	}
	out := []usageRow{}
	for rows.Next() {
		var u usageRow
		if err := rows.Scan(&u.Label, &u.KeyID, &u.Requests, &u.Errors,
			&u.PromptTokens, &u.CompletionTokens, &u.CachedTokens, &u.AvgLatencyMS, &u.AvgTTFTMS); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{"by": by, "since": since, "rows": out})
}

func (s *Server) channels(w http.ResponseWriter, _ *http.Request, _ string) {
	cfg := s.deps.Cfg
	type modelHealth struct {
		Name    string `json:"name"`
		Ref     string `json:"ref"`
		Healthy bool   `json:"healthy"`
	}
	type provView struct {
		Name            string        `json:"name"`
		BaseURL         string        `json:"base_url"`
		Target          string        `json:"target"`
		Timeout         string        `json:"timeout"`
		HasSystemPrompt bool          `json:"has_system_prompt"`
		HasExtraBody    bool          `json:"has_extra_body"`
		Models          []modelHealth `json:"models"`
	}
	provs := make([]provView, 0, len(cfg.Providers))
	for _, p := range cfg.Providers {
		ms := make([]modelHealth, 0, len(p.Models))
		for _, m := range p.Models {
			ref := p.Name + "/" + m
			ms = append(ms, modelHealth{
				Name:    m,
				Ref:     ref,
				Healthy: s.deps.Reg == nil || s.deps.Reg.Healthy(ref),
			})
		}
		base := p.BaseURL
		if p.Plugin != "" {
			base = "plugin:" + p.Plugin
		}
		provs = append(provs, provView{
			Name:            p.Name,
			BaseURL:         base,
			Target:          p.Target,
			Timeout:         p.Timeout.D(300 * time.Second).String(),
			HasSystemPrompt: p.SystemPrompt != "",
			HasExtraBody:    len(p.ExtraBody) > 0,
			Models:          ms,
		})
	}

	type groupView struct {
		Name       string             `json:"name"`
		Type       string             `json:"type"`
		Default    bool               `json:"default"`
		Members    []string           `json:"members"`
		Weights    map[string]float64 `json:"weights,omitempty"`
		Classifier string             `json:"classifier,omitempty"`
		RulesCount int                `json:"rules_count"`
	}
	gnames := make([]string, 0, len(cfg.RoutingGroups))
	for n := range cfg.RoutingGroups {
		gnames = append(gnames, n)
	}
	sort.Strings(gnames)
	groups := make([]groupView, 0, len(gnames))
	for _, n := range gnames {
		g := cfg.RoutingGroups[n]
		groups = append(groups, groupView{
			Name:       n,
			Type:       g.Type,
			Default:    n == cfg.DefaultGroup,
			Members:    g.Members,
			Weights:    g.Weights,
			Classifier: g.Classifier,
			RulesCount: len(g.Rules),
		})
	}

	type hourStat struct {
		Model        string  `json:"model"`
		Requests     int64   `json:"requests"`
		Errors       int64   `json:"errors"`
		SuccessRate  float64 `json:"success_rate"`
		AvgLatencyMS float64 `json:"avg_latency_ms"`
		AvgTTFTMS    float64 `json:"avg_ttft_ms"`
	}
	stats := []hourStat{}
	cutoff := time.Now().Add(-time.Hour).Unix()
	rows, err := s.deps.DB.Query(`
		SELECT model, COUNT(*), COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
		       COALESCE(AVG(latency_ms),0), COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)
		FROM request_log WHERE ts >= ? AND model != ''
		GROUP BY model ORDER BY COUNT(*) DESC`, cutoff)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var st hourStat
			if rows.Scan(&st.Model, &st.Requests, &st.Errors, &st.AvgLatencyMS, &st.AvgTTFTMS) == nil {
				if st.Requests > 0 {
					st.SuccessRate = float64(st.Requests-st.Errors) * 100 / float64(st.Requests)
				}
				stats = append(stats, st)
			}
		}
	}

	aliases := cfg.ModelAliases
	if aliases == nil {
		aliases = []config.ModelAlias{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"providers":      provs,
		"routing_groups": groups,
		"model_aliases":  aliases,
		"default_group":  cfg.DefaultGroup,
		"hourly_stats":   stats,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}
