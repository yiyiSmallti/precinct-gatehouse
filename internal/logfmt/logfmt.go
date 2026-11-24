// Package logfmt renders the gateway's numeric log fields with units, so a
// line reads "est=48.2k dur=3.4s" instead of "est=48213 ms=3412".
package logfmt

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"
)

// tokenKeys are the attribute keys whose integer values are token counts.
// Counters such as hits/count/removed are left as plain numbers.
var tokenKeys = map[string]bool{
	"tokens":         true,
	"est":            true,
	"est_tokens":     true,
	"orig":           true,
	"orig_est":       true,
	"cached_est":     true,
	"miss_est":       true,
	"src_est":        true,
	"summary_est":    true,
	"comp":           true,
	"stage1":         true,
	"prompt":         true,
	"cached":         true,
	"actual_prompt":  true,
	"actual_cached":  true,
	"prewarm_prompt": true,
	"prewarm_cached": true,
}

// ReplaceAttr is a slog.HandlerOptions.ReplaceAttr that formats token counts
// (1234 -> 1.2k) and millisecond durations (key "ms" becomes "dur": 850ms,
// 3.4s, 2m05s). Non-integer values pass through untouched.
func ReplaceAttr(_ []string, a slog.Attr) slog.Attr {
	n, ok := intValue(a.Value)
	if !ok {
		return a
	}
	switch {
	case a.Key == "ms":
		return slog.String("dur", Millis(n))
	case tokenKeys[a.Key]:
		return slog.String(a.Key, Tokens(n))
	}
	return a
}

func intValue(v slog.Value) (int64, bool) {
	switch v.Kind() {
	case slog.KindInt64:
		return v.Int64(), true
	case slog.KindUint64:
		u := v.Uint64()
		if u > 1<<62 {
			return 0, false
		}
		return int64(u), true
	}
	return 0, false
}

// Tokens formats a token count: below 1000 as is, then k and M with one
// decimal (trailing ".0" dropped): 850, 1.2k, 48k, 1.1M.
func Tokens(n int64) string {
	abs := n
	if abs < 0 {
		abs = -abs
	}
	switch {
	case abs < 1000:
		return strconv.FormatInt(n, 10)
	case abs < 999_950:
		return oneDecimal(float64(n)/1e3) + "k"
	default:
		return oneDecimal(float64(n)/1e6) + "M"
	}
}

// Millis formats a millisecond duration: 850ms, 3.4s, 2m05s, 1h02m.
func Millis(ms int64) string {
	if ms < 0 {
		return strconv.FormatInt(ms, 10) + "ms"
	}
	switch {
	case ms < 1000:
		return strconv.FormatInt(ms, 10) + "ms"
	case ms < 59_950:
		return oneDecimal(float64(ms)/1e3) + "s"
	}
	sec := (ms + 500) / 1000
	if sec < 3600 {
		return fmt.Sprintf("%dm%02ds", sec/60, sec%60)
	}
	return fmt.Sprintf("%dh%02dm", sec/3600, (sec%3600)/60)
}

func oneDecimal(f float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(f, 'f', 1, 64), ".0")
}
