package admin

import (
	"bufio"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/limit"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact/rules"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/tiered"
)

//go:embed ui
var uiFiles embed.FS

// Deps holds the dependencies required by the admin server.
type Deps struct {
	DB               *sql.DB
	Keys             *keys.Store
	Limiter          *limit.Limiter
	Live             *live.Manager
	Tiered           *tiered.Hub
	MissingDetectors []string
	UserKey          []byte
	SessionTTL       time.Duration
}

// Server serves the /admin/ Web UI and /admin/api/* JSON API.
type Server struct {
	deps  Deps
	auth  *authStore
	setup setupState
	ui    fs.FS
	mux   *http.ServeMux
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
	s.mux.HandleFunc("GET /admin/", s.handleStatic())

	s.mux.HandleFunc("POST /admin/api/login", s.login)
	s.mux.HandleFunc("GET /admin/api/setup", s.setupStatus)
	s.mux.HandleFunc("POST /admin/api/setup", s.completeSetup)
	s.mux.HandleFunc("POST /admin/api/logout", s.guard(s.logout))
	s.mux.HandleFunc("GET /admin/api/me", s.guard(s.me))
	s.mux.HandleFunc("GET /admin/api/overview", s.guard(s.overview))
	s.mux.HandleFunc("GET /admin/api/keys", s.guard(s.listKeys))
	s.mux.HandleFunc("POST /admin/api/keys", s.guard(s.createKey))
	s.mux.HandleFunc("PATCH /admin/api/keys/{id}", s.guard(s.updateKey))
	s.mux.HandleFunc("POST /admin/api/keys/{id}/reset", s.guard(s.resetKey))
	s.mux.HandleFunc("POST /admin/api/keys/{id}/reset-usage", s.guard(s.resetKeyUsage))
	s.mux.HandleFunc("DELETE /admin/api/keys/{id}", s.guard(s.deleteKey))
	s.mux.HandleFunc("GET /admin/api/logs", s.guard(s.logs))
	s.mux.HandleFunc("GET /admin/api/usage", s.guard(s.usage))
	s.mux.HandleFunc("GET /admin/api/stats", s.guard(s.stats))
	s.mux.HandleFunc("GET /admin/api/channels", s.guard(s.channels))
	s.mux.HandleFunc("GET /admin/api/redaction", s.guard(s.redactionInfo))
	s.mux.HandleFunc("POST /admin/api/redaction/try", s.guard(s.redactionTry))
	s.mux.HandleFunc("GET /admin/api/config", s.guard(s.getConfig))
	s.mux.HandleFunc("PUT /admin/api/config/sections", s.guard(s.putConfigSections))
	s.mux.HandleFunc("PUT /admin/api/config/sections/{section}", s.guard(s.putConfigSection))
	s.mux.HandleFunc("GET /admin/api/config/yaml", s.guard(s.getConfigYAML))
	s.mux.HandleFunc("PUT /admin/api/config/yaml", s.guard(s.putConfigYAML))
	s.mux.HandleFunc("POST /admin/api/config/validate", s.guard(s.validateConfig))
	s.mux.HandleFunc("POST /admin/api/config/reload", s.guard(s.reloadConfig))
	s.mux.HandleFunc("GET /admin/api/config/history", s.guard(s.listConfigHistory))
	s.mux.HandleFunc("GET /admin/api/config/history/{name}", s.guard(s.getConfigHistory))
	s.mux.HandleFunc("POST /admin/api/config/rollback", s.guard(s.rollbackConfig))
	s.mux.HandleFunc("POST /admin/api/providers/test", s.guard(s.testProvider))
	s.mux.HandleFunc("PUT /admin/api/providers/{name}/key", s.guard(s.putProviderKey))
	s.mux.HandleFunc("DELETE /admin/api/providers/{name}/key", s.guard(s.deleteProviderKey))
	s.mux.HandleFunc("POST /admin/api/password", s.guard(s.changePassword))
	s.mux.HandleFunc("GET /admin/api/flow", s.guard(s.flow))
	s.mux.HandleFunc("GET /admin/api/tiered", s.guard(s.getTiered))
	s.mux.HandleFunc("GET /admin/api/tiered/{provider}/starters", s.guard(s.getTieredStarters))
	s.mux.HandleFunc("POST /admin/api/tiered/{provider}/starters/pin", s.guard(s.pinTieredStarter))
	s.mux.HandleFunc("DELETE /admin/api/tiered/{provider}/starters/{hash}", s.guard(s.deleteTieredStarter))
	s.mux.HandleFunc("POST /admin/api/tiered/{provider}/starters/warm", s.guard(s.warmTieredStarters))
	s.mux.HandleFunc("POST /admin/api/tiered/{provider}/shadow/reset", s.guard(s.resetTieredShadow))
	s.mux.HandleFunc("GET /admin/api/tiered/{provider}/checkpoints", s.guard(s.getTieredCheckpoints))
	s.mux.HandleFunc("DELETE /admin/api/tiered/{provider}/checkpoints", s.guard(s.deleteTieredCheckpoints))
	s.mux.HandleFunc("DELETE /admin/api/tiered/{provider}/checkpoints/{id}", s.guard(s.deleteTieredCheckpoint))
}

func (s *Server) sessionTTL() time.Duration {
	if s.deps.Live != nil {
		if snap := s.deps.Live.Current(); snap != nil && snap.Cfg != nil {
			return snap.Cfg.Admin.SessionTTL.D(12 * time.Hour)
		}
	}
	return s.auth.ttl
}

func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request, tok string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = s.sessionTTL()
	}
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    tok,
		Path:     "/admin",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   isHTTPS(r),
		MaxAge:   int(ttl.Seconds()),
	})
}

func (s *Server) guard(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		storedPass, errP := getSetting(s.deps.DB, "admin_pass")
		if errP != nil || storedPass == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		user, ok := s.auth.verifySession(c.Value, storedPass, s.sessionTTL(), time.Now())
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
	ttl := s.sessionTTL()
	tok, err := s.auth.createSession(storedUser, storedPass, ttl, now)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.setSessionCookie(w, r, tok, ttl)
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
	curHour := localHour(now)
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

	TokensToday int64 `json:"tokens_today"`
	TokensMonth int64 `json:"tokens_month"`
	TokensUsed  int64 `json:"tokens_used"` // lifetime, compared with tokens_total
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

		TokensToday: snap.TokensToday,
		TokensMonth: snap.TokensMonth,
		TokensUsed:  snap.TokensTotal,
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

		TokensDaily   int64 `json:"tokens_daily"`
		TokensMonthly int64 `json:"tokens_monthly"`
		TokensTotal   int64 `json:"tokens_total"`
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

		TokensDaily:   req.TokensDaily,
		TokensMonthly: req.TokensMonthly,
		TokensTotal:   req.TokensTotal,
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

		TokensDaily   *int64 `json:"tokens_daily"`
		TokensMonthly *int64 `json:"tokens_monthly"`
		TokensTotal   *int64 `json:"tokens_total"`
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

		TokensDaily:   req.TokensDaily,
		TokensMonthly: req.TokensMonthly,
		TokensTotal:   req.TokensTotal,
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

// resetKeyUsage clears the token ledger of a key so its quotas start over.
func (s *Server) resetKeyUsage(w http.ResponseWriter, r *http.Request, _ string) {
	id := r.PathValue("id")
	k, err := s.deps.Keys.Get(id)
	if errors.Is(err, keys.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := s.deps.Limiter.ResetTokens(id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"api_key": s.makeKeyView(k, time.Now())})
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
		status,error_code,stream,prompt_tokens,completion_tokens,cached_tokens,latency_ms,ttft_ms,admitted,
		redaction_mode,redaction_count,route_backend,route_reason
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
		RedactionMode    string `json:"redaction_mode"`
		RedactionCount   int    `json:"redaction_count"`
		RouteBackend     string `json:"route_backend"`
		RouteReason      string `json:"route_reason"`
	}
	out := []logRow{}
	hasMore := false
	for rows.Next() {
		var row logRow
		var streamInt, admInt int
		if err := rows.Scan(&row.ID, &row.RequestID, &row.TS, &row.Day, &row.KeyID, &row.KeyName,
			&row.Protocol, &row.ClientIP, &row.RequestedModel, &row.Model,
			&row.Status, &row.ErrorCode, &streamInt, &row.PromptTokens, &row.CompletionTokens,
			&row.CachedTokens, &row.LatencyMS, &row.TTFTMS, &admInt,
			&row.RedactionMode, &row.RedactionCount, &row.RouteBackend, &row.RouteReason); err != nil {
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
	snap := s.deps.Live.Current()
	cfg := snap.Cfg
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
		HasTiered       bool          `json:"has_tiered"`
		TieredMode      string        `json:"tiered_mode,omitempty"`
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
				Healthy: snap.Reg == nil || snap.Reg.Healthy(ref),
			})
		}
		base := p.BaseURL
		if p.Plugin != "" {
			base = "plugin:" + p.Plugin
		} else if base == "" && p.Tiered != nil {
			base = "tiered:" + p.Tiered.Mode
		}
		tmode := ""
		if p.Tiered != nil {
			tmode = p.Tiered.Mode
		}
		provs = append(provs, provView{
			Name:            p.Name,
			BaseURL:         base,
			Target:          p.Target,
			Timeout:         p.Timeout.D(300 * time.Second).String(),
			HasSystemPrompt: p.SystemPrompt != "",
			HasExtraBody:    len(p.ExtraBody) > 0,
			HasTiered:       p.Tiered != nil,
			TieredMode:      tmode,
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

func redactionMode(snap *live.Snapshot, target, class string) string {
	if snap.Redactor != nil {
		return snap.Redactor.Mode(target, class)
	}
	if class == "confidential" && target == "external" {
		return redact.ModeBlock
	}
	if m := snap.Cfg.Redaction.ModeByTarget[target]; m != "" {
		return m
	}
	return redact.ModeMask
}

func (s *Server) redactionInfo(w http.ResponseWriter, _ *http.Request, _ string) {
	snap := s.deps.Live.Current()
	rc := snap.Cfg.Redaction
	modeByTarget := map[string]string{
		"local":    redactionMode(snap, "local", "internal"),
		"external": redactionMode(snap, "external", "internal"),
	}
	for k, v := range rc.ModeByTarget {
		modeByTarget[k] = v
	}

	classes := []string{"public", "internal", "confidential"}
	targets := []string{"local", "external"}
	matrix := map[string]map[string]string{}
	for _, tgt := range targets {
		row := map[string]string{}
		for _, cl := range classes {
			row[cl] = redactionMode(snap, tgt, cl)
		}
		matrix[tgt] = row
	}

	type catEntry struct {
		Key         string `json:"key"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Action      string `json:"action"`
		DefaultUsed bool   `json:"default_used"`
	}
	builtins := []struct {
		key, name, desc string
	}{
		{strings.ToLower(rules.Phone), rules.Phone, "中国大陆手机号"},
		{strings.ToLower(rules.IDCard), rules.IDCard, "居民身份证号"},
		{strings.ToLower(rules.BankCard), rules.BankCard, "银行卡号"},
		{strings.ToLower(rules.USCC), rules.USCC, "统一社会信用代码"},
		{strings.ToLower(rules.Email), rules.Email, "电子邮箱"},
		{strings.ToLower(rules.Plate), rules.Plate, "车牌号"},
		{strings.ToLower(rules.IP), rules.IP, "内网 IP 地址"},
		{strings.ToLower(rules.Domain), rules.Domain, "内网域名"},
		{strings.ToLower(rules.Secret), rules.Secret, "密钥与令牌"},
		{strings.ToLower(redact.Term), redact.Term, "自定义词典词条"},
		{"image", "image", "图像输入"},
	}
	seenCats := map[string]bool{}
	cats := make([]catEntry, 0, len(builtins)+len(rc.Categories))
	for _, b := range builtins {
		seenCats[b.key] = true
		act := rc.Categories[b.key]
		defUsed := false
		if act == "" {
			act = "mask"
			defUsed = true
		}
		cats = append(cats, catEntry{Key: b.key, Name: b.name, Description: b.desc, Action: act, DefaultUsed: defUsed})
	}
	var extraKeys []string
	for k := range rc.Categories {
		if !seenCats[k] {
			extraKeys = append(extraKeys, k)
		}
	}
	sort.Strings(extraKeys)
	for _, k := range extraKeys {
		cats = append(cats, catEntry{Key: k, Name: strings.ToUpper(k), Description: "自定义类别", Action: rc.Categories[k]})
	}

	dictCount := 0
	dictErr := ""
	if rc.Dictionary != "" {
		f, err := os.Open(rc.Dictionary)
		if err != nil {
			dictErr = err.Error()
		} else {
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				t := strings.TrimSpace(sc.Text())
				if t != "" && !strings.HasPrefix(t, "#") {
					dictCount++
				}
			}
			if err := sc.Err(); err != nil {
				dictErr = err.Error()
			}
			f.Close()
		}
	}
	termsCount := 0
	for _, t := range rc.Terms {
		if strings.TrimSpace(t) != "" {
			termsCount++
		}
	}

	neverMask := rc.NeverMask
	if neverMask == nil {
		neverMask = []string{}
	}
	internalDomains := rc.InternalDomains
	if len(internalDomains) == 0 {
		internalDomains = []string{".internal", ".corp", ".lan", ".local", ".intra"}
	}
	detectors := rc.Detectors
	if detectors == nil {
		detectors = []string{}
	}
	var missing []string
	if snap.Redactor != nil {
		missing = snap.Redactor.Missing()
	} else {
		missing = s.deps.MissingDetectors
	}
	if missing == nil {
		missing = []string{}
	}
	stripFields := append([]string{"dsh_session_log"}, rc.StripFields...)

	type provMode struct {
		Name          string `json:"name"`
		Target        string `json:"target"`
		EffectiveMode string `json:"effective_mode"`
	}
	provs := make([]provMode, 0, len(snap.Cfg.Providers))
	for _, p := range snap.Cfg.Providers {
		provs = append(provs, provMode{
			Name:          p.Name,
			Target:        p.Target,
			EffectiveMode: redactionMode(snap, p.Target, "internal"),
		})
	}

	type modeStat struct {
		Mode     string `json:"mode"`
		Requests int64  `json:"requests"`
		Count    int64  `json:"count"`
	}
	stats := []modeStat{}
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	rows, err := s.deps.DB.Query(`
		SELECT redaction_mode, COUNT(*), COALESCE(SUM(redaction_count), 0)
		FROM request_log
		WHERE ts >= ? AND redaction_mode != ''
		GROUP BY redaction_mode ORDER BY COUNT(*) DESC`, cutoff)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var ms modeStat
			if rows.Scan(&ms.Mode, &ms.Requests, &ms.Count) == nil {
				stats = append(stats, ms)
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"mode_by_target": modeByTarget,
		"matrix":         matrix,
		"categories":     cats,
		"dictionary": map[string]any{
			"path":  rc.Dictionary,
			"count": dictCount,
			"error": dictErr,
		},
		"terms_count":           termsCount,
		"never_mask":            neverMask,
		"internal_domains":      internalDomains,
		"detectors":             detectors,
		"detectors_unavailable": missing,
		"fail_closed":           rc.FailClosedOn(),
		"strip_fields":          stripFields,
		"vault_ttl":             rc.VaultTTL.D(24 * time.Hour).String(),
		"providers":             provs,
		"stats_24h":             stats,
	})
}

func (s *Server) redactionTry(w http.ResponseWriter, r *http.Request, _ string) {
	snap := s.deps.Live.Current()
	if snap.Redactor == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "redactor unavailable")
		return
	}
	var req struct {
		Text      string `json:"text"`
		Target    string `json:"target"`
		DataClass string `json:"data_class"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 128<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if len(req.Text) > 64<<10 {
		writeJSONError(w, http.StatusBadRequest, "text exceeds 64KB limit")
		return
	}
	if req.Target == "" {
		req.Target = "external"
	}
	if req.Target != "local" && req.Target != "external" {
		writeJSONError(w, http.StatusBadRequest, "target must be local or external")
		return
	}
	if req.DataClass == "" {
		req.DataClass = "internal"
	}
	if req.DataClass != "public" && req.DataClass != "internal" && req.DataClass != "confidential" {
		writeJSONError(w, http.StatusBadRequest, "data_class must be public, internal or confidential")
		return
	}

	mode := snap.Redactor.Mode(req.Target, req.DataClass)
	oaiReq := oai.Object{"messages": []any{map[string]any{"role": "user", "content": req.Text}}}
	res, aerr := snap.Redactor.Apply(r.Context(), oaiReq, mode, s.deps.UserKey, "admin-try")
	outText := ""
	errMsg := ""
	if aerr != nil {
		errMsg = aerr.Error()
	} else {
		outText = oai.Text(oai.Messages(oaiReq)[0])
	}
	var report any
	if res != nil {
		report = res.Report
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":   mode,
		"text":   outText,
		"report": report,
		"error":  errMsg,
	})
}
