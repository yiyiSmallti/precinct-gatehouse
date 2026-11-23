package admin

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// statsBucket is one point of the stats time series.
type statsBucket struct {
	T                string  `json:"t"` // YYYY-MM-DD, or YYYY-MM-DD HH:00 for hourly buckets
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
	AvgTTFTMS        float64 `json:"avg_ttft_ms"`
}

type statsTotals struct {
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	RateLimited      int64   `json:"rate_limited"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
	AvgTTFTMS        float64 `json:"avg_ttft_ms"`
	ActiveKeys       int64   `json:"active_keys"`
	ActiveModels     int64   `json:"active_models"`
}

type statsSlice struct {
	Label            string  `json:"label"`
	ID               string  `json:"id,omitempty"`
	Requests         int64   `json:"requests"`
	Errors           int64   `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	AvgLatencyMS     float64 `json:"avg_latency_ms"`
}

// stats serves GET /admin/api/stats?days=1|7|14|30|90&key=<key_id>&model=<model>.
// days=1 returns 24 hourly buckets; longer ranges return daily buckets. Every
// bucket in the range is present (zero-filled) so charts need no gap logic.
func (s *Server) stats(w http.ResponseWriter, r *http.Request, _ string) {
	q := r.URL.Query()
	days := 14
	if v, err := strconv.Atoi(q.Get("days")); err == nil && v > 0 {
		days = min(v, 366)
	}
	now := time.Now().Local()
	hourly := days == 1
	var start time.Time
	if hourly {
		start = now.Truncate(time.Hour).Add(-23 * time.Hour)
	} else {
		start = time.Date(now.Year(), now.Month(), now.Day()-days+1, 0, 0, 0, 0, now.Location())
	}
	prevStart := start.Add(-now.Sub(start))

	where := []string{"ts >= ?"}
	args := []any{start.Unix()}
	var filter []string
	var fargs []any
	if k := strings.TrimSpace(q.Get("key")); k != "" {
		filter = append(filter, "key_id = ?")
		fargs = append(fargs, k)
	}
	if m := strings.TrimSpace(q.Get("model")); m != "" {
		filter = append(filter, "model = ?")
		fargs = append(fargs, m)
	}
	where = append(where, filter...)
	args = append(args, fargs...)
	cond := strings.Join(where, " AND ")

	db := s.deps.DB
	fail := func(err error) { writeJSONError(w, http.StatusInternalServerError, err.Error()) }

	const aggCols = `COUNT(*),
		COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
		COALESCE(AVG(CASE WHEN status<400 THEN latency_ms END),0),
		COALESCE(AVG(CASE WHEN ttft_ms>0 THEN ttft_ms END),0)`

	totals := func(c string, a []any) (statsTotals, error) {
		var t statsTotals
		err := db.QueryRow(`SELECT `+aggCols+`,
			COALESCE(SUM(CASE WHEN status=429 THEN 1 ELSE 0 END),0),
			COUNT(DISTINCT NULLIF(key_id,'')), COUNT(DISTINCT NULLIF(model,''))
			FROM request_log WHERE `+c, a...).Scan(
			&t.Requests, &t.Errors, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens,
			&t.AvgLatencyMS, &t.AvgTTFTMS, &t.RateLimited, &t.ActiveKeys, &t.ActiveModels)
		return t, err
	}
	cur, err := totals(cond, args)
	if err != nil {
		fail(err)
		return
	}
	prevCond := strings.Join(append([]string{"ts >= ? AND ts < ?"}, filter...), " AND ")
	prev, err := totals(prevCond, append([]any{prevStart.Unix(), start.Unix()}, fargs...))
	if err != nil {
		fail(err)
		return
	}

	// Time series, zero-filled.
	bucketExpr := `day`
	if hourly {
		bucketExpr = `strftime('%Y-%m-%d %H:00', ts, 'unixepoch', 'localtime')`
	}
	series := []statsBucket{}
	idx := map[string]int{}
	if hourly {
		for t := start; !t.After(now); t = t.Add(time.Hour) {
			label := t.Format("2006-01-02 15:00")
			idx[label] = len(series)
			series = append(series, statsBucket{T: label})
		}
	} else {
		for t := start; !t.After(now); t = t.AddDate(0, 0, 1) {
			label := t.Format("2006-01-02")
			idx[label] = len(series)
			series = append(series, statsBucket{T: label})
		}
	}
	rows, err := db.Query(`SELECT `+bucketExpr+` AS b, `+aggCols+` FROM request_log WHERE `+cond+` GROUP BY b`, args...)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var b statsBucket
		if err := rows.Scan(&b.T, &b.Requests, &b.Errors, &b.PromptTokens, &b.CompletionTokens, &b.CachedTokens, &b.AvgLatencyMS, &b.AvgTTFTMS); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if i, ok := idx[b.T]; ok {
			series[i] = b
		}
	}
	rows.Close()

	slices := func(labelExpr, idExpr string, limit int) ([]statsSlice, error) {
		rows, err := db.Query(`SELECT `+labelExpr+` AS l, `+idExpr+` AS i, COUNT(*),
			COALESCE(SUM(CASE WHEN status>=400 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
			COALESCE(AVG(CASE WHEN status<400 THEN latency_ms END),0)
			FROM request_log WHERE `+cond+` GROUP BY l, i
			ORDER BY SUM(prompt_tokens+completion_tokens) DESC, COUNT(*) DESC LIMIT ?`, append(args, limit)...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []statsSlice{}
		for rows.Next() {
			var sl statsSlice
			if err := rows.Scan(&sl.Label, &sl.ID, &sl.Requests, &sl.Errors, &sl.PromptTokens, &sl.CompletionTokens, &sl.CachedTokens, &sl.AvgLatencyMS); err != nil {
				return nil, err
			}
			out = append(out, sl)
		}
		return out, rows.Err()
	}
	byModel, err := slices(`COALESCE(NULLIF(model,''), NULLIF(requested_model,''), '(unknown)')`, `''`, 12)
	if err != nil {
		fail(err)
		return
	}
	byKey, err := slices(`COALESCE(NULLIF(key_name,''), NULLIF(key_id,''), '(anonymous)')`, `key_id`, 12)
	if err != nil {
		fail(err)
		return
	}
	byProtocol, err := slices(`COALESCE(NULLIF(protocol,''), 'chat')`, `''`, 8)
	if err != nil {
		fail(err)
		return
	}

	// Status classes.
	status := map[string]int64{"2xx": 0, "4xx": 0, "429": 0, "5xx": 0}
	rows, err = db.Query(`SELECT CASE WHEN status=429 THEN '429' WHEN status>=500 THEN '5xx'
		WHEN status>=400 THEN '4xx' ELSE '2xx' END AS c, COUNT(*) FROM request_log WHERE `+cond+` GROUP BY c`, args...)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var c string
		var n int64
		if err := rows.Scan(&c, &n); err != nil {
			rows.Close()
			fail(err)
			return
		}
		status[c] = n
	}
	rows.Close()

	// Weekday × hour activity (0 = Sunday, local time).
	heat := make([][]int64, 7)
	for i := range heat {
		heat[i] = make([]int64, 24)
	}
	rows, err = db.Query(`SELECT CAST(strftime('%w', ts, 'unixepoch', 'localtime') AS INTEGER),
		CAST(strftime('%H', ts, 'unixepoch', 'localtime') AS INTEGER), COUNT(*)
		FROM request_log WHERE `+cond+` GROUP BY 1, 2`, args...)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var d, h int
		var n int64
		if err := rows.Scan(&d, &h, &n); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if d >= 0 && d < 7 && h >= 0 && h < 24 {
			heat[d][h] = n
		}
	}
	rows.Close()

	// Latency distribution of successful requests.
	type latBucket struct {
		Label string `json:"label"`
		Count int64  `json:"count"`
	}
	edges := []struct {
		label string
		max   int64
	}{{"<0.5s", 500}, {"0.5–1s", 1000}, {"1–2s", 2000}, {"2–5s", 5000}, {"5–10s", 10000}, {"10–30s", 30000}, {"30–60s", 60000}, {">60s", -1}}
	caseExpr := "CASE"
	for i, e := range edges {
		if e.max < 0 {
			caseExpr += " ELSE " + strconv.Itoa(i) + " END"
		} else {
			caseExpr += " WHEN latency_ms < " + strconv.FormatInt(e.max, 10) + " THEN " + strconv.Itoa(i)
		}
	}
	latency := make([]latBucket, len(edges))
	for i, e := range edges {
		latency[i].Label = e.label
	}
	rows, err = db.Query(`SELECT `+caseExpr+` AS b, COUNT(*) FROM request_log WHERE `+cond+` AND status<400 GROUP BY b`, args...)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var b int
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			rows.Close()
			fail(err)
			return
		}
		if b >= 0 && b < len(latency) {
			latency[b].Count = n
		}
	}
	rows.Close()

	bucket := "day"
	if hourly {
		bucket = "hour"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"days":        days,
		"bucket":      bucket,
		"since":       start.Format(time.RFC3339),
		"totals":      cur,
		"prev_totals": prev,
		"series":      series,
		"by_model":    byModel,
		"by_key":      byKey,
		"by_protocol": byProtocol,
		"status":      status,
		"heatmap":     heat,
		"latency":     latency,
	})
}
