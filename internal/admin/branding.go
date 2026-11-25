package admin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	settingBrandingDisplayName  = "branding_display_name"
	settingBrandingLogoData     = "branding_logo_data"
	settingBrandingLogoMime     = "branding_logo_mime"
	settingBrandingLogoVer      = "branding_logo_ver"
	settingBrandingPublicURL    = "branding_public_url"
	settingBrandingDistTemplate = "branding_dist_template"

	maxDisplayNameRunes  = 80
	maxPublicURLLen      = 512
	maxDistTemplateBytes = 16 << 10
	maxLogoBytes         = 2 << 20
)

var svgEventAttrRe = regexp.MustCompile(`(?i)\bon[a-z]+\s*=`)

type brandingPublic struct {
	DisplayName string `json:"display_name"`
	HasLogo     bool   `json:"has_logo"`
	LogoURL     string `json:"logo_url"`
}

type brandingSettings struct {
	brandingPublic
	PublicURL    string `json:"public_url"`
	DistTemplate string `json:"dist_template"`
}

func getSettingOptional(db *sql.DB, k string) string {
	if db == nil {
		return ""
	}
	v, err := getSetting(db, k)
	if err != nil {
		return ""
	}
	return v
}

func saveOrDeleteSetting(db *sql.DB, k, v string) error {
	if v == "" {
		_, err := db.Exec(`DELETE FROM settings WHERE k=?`, k)
		return err
	}
	return setSetting(db, k, v)
}

func (s *Server) loadPublicBranding() brandingPublic {
	name := getSettingOptional(s.deps.DB, settingBrandingDisplayName)
	logoData := getSettingOptional(s.deps.DB, settingBrandingLogoData)
	logoMime := getSettingOptional(s.deps.DB, settingBrandingLogoMime)
	logoVer := getSettingOptional(s.deps.DB, settingBrandingLogoVer)
	hasLogo := logoData != "" && logoMime != ""
	logoURL := ""
	if hasLogo {
		logoURL = "/admin/api/branding/logo"
		if logoVer != "" {
			logoURL += "?v=" + url.QueryEscape(logoVer)
		}
	}
	return brandingPublic{
		DisplayName: name,
		HasLogo:     hasLogo,
		LogoURL:     logoURL,
	}
}

func (s *Server) loadBrandingSettings() brandingSettings {
	return brandingSettings{
		brandingPublic: s.loadPublicBranding(),
		PublicURL:      getSettingOptional(s.deps.DB, settingBrandingPublicURL),
		DistTemplate:   getSettingOptional(s.deps.DB, settingBrandingDistTemplate),
	}
}

func (s *Server) getPublicBranding(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.loadPublicBranding())
}

func (s *Server) getBrandingLogo(w http.ResponseWriter, r *http.Request) {
	logoData := getSettingOptional(s.deps.DB, settingBrandingLogoData)
	logoMime := getSettingOptional(s.deps.DB, settingBrandingLogoMime)
	logoVer := getSettingOptional(s.deps.DB, settingBrandingLogoVer)
	if logoData == "" || logoMime == "" {
		http.NotFound(w, r)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(logoData)
	if err != nil || len(raw) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", logoMime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	if v := r.URL.Query().Get("v"); v != "" && v == logoVer {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}

func (s *Server) getBrandingSettings(w http.ResponseWriter, _ *http.Request, _ string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.loadBrandingSettings())
}

func (s *Server) putBrandingSettings(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	var req struct {
		DisplayName  *string `json:"display_name"`
		PublicURL    *string `json:"public_url"`
		DistTemplate *string `json:"dist_template"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if req.DisplayName != nil {
		name := strings.TrimSpace(*req.DisplayName)
		if utf8.RuneCountInString(name) > maxDisplayNameRunes {
			writeJSONError(w, http.StatusBadRequest, "display_name exceeds 80 characters")
			return
		}
		if err := saveOrDeleteSetting(s.deps.DB, settingBrandingDisplayName, name); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if req.PublicURL != nil {
		pubURL := strings.TrimRight(strings.TrimSpace(*req.PublicURL), "/")
		if len(pubURL) > maxPublicURLLen {
			writeJSONError(w, http.StatusBadRequest, "public_url is too long")
			return
		}
		if pubURL != "" {
			u, err := url.Parse(pubURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				writeJSONError(w, http.StatusBadRequest, "public_url must be an http:// or https:// URL")
				return
			}
		}
		if err := saveOrDeleteSetting(s.deps.DB, settingBrandingPublicURL, pubURL); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if req.DistTemplate != nil {
		tpl := *req.DistTemplate
		if strings.TrimSpace(tpl) == "" {
			tpl = ""
		}
		if len(tpl) > maxDistTemplateBytes {
			writeJSONError(w, http.StatusBadRequest, "dist_template exceeds 16 KiB limit")
			return
		}
		if err := saveOrDeleteSetting(s.deps.DB, settingBrandingDistTemplate, tpl); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.loadBrandingSettings())
}

func (s *Server) putBrandingLogo(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	var req struct {
		DataURL string `json:"data_url"`
		Mime    string `json:"mime"`
		Data    string `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid logo payload")
		return
	}
	mimeType, raw, err := validateAndDecodeLogo(req.DataURL, req.Mime, req.Data)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	sum := sha256.Sum256(raw)
	ver := hex.EncodeToString(sum[:])[:12]
	b64 := base64.StdEncoding.EncodeToString(raw)

	tx, err := s.deps.DB.Begin()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	for _, kv := range [][2]string{
		{settingBrandingLogoData, b64},
		{settingBrandingLogoMime, mimeType},
		{settingBrandingLogoVer, ver},
	} {
		if _, err := tx.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, kv[0], kv[1]); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.loadPublicBranding())
}

func (s *Server) deleteBrandingLogo(w http.ResponseWriter, _ *http.Request, _ string) {
	if s.deps.DB == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	tx, err := s.deps.DB.Begin()
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback()
	for _, k := range []string{settingBrandingLogoData, settingBrandingLogoMime, settingBrandingLogoVer} {
		if _, err := tx.Exec(`DELETE FROM settings WHERE k=?`, k); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if err := tx.Commit(); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, s.loadPublicBranding())
}

func validateAndDecodeLogo(dataURL, rawMime, rawData string) (string, []byte, error) {
	declaredMime := strings.ToLower(strings.TrimSpace(rawMime))
	b64 := strings.TrimSpace(rawData)
	if dataURL != "" {
		rest, ok := strings.CutPrefix(strings.TrimSpace(dataURL), "data:")
		if !ok {
			return "", nil, errors.New("data_url must start with data:")
		}
		meta, payload, ok := strings.Cut(rest, ",")
		if !ok {
			return "", nil, errors.New("invalid data_url format")
		}
		lowerMeta := strings.ToLower(meta)
		if !strings.HasSuffix(lowerMeta, ";base64") {
			return "", nil, errors.New("data_url must be base64-encoded")
		}
		declaredMime = strings.TrimSpace(strings.TrimSuffix(lowerMeta, ";base64"))
		if idx := strings.IndexByte(declaredMime, ';'); idx >= 0 {
			declaredMime = strings.TrimSpace(declaredMime[:idx])
		}
		b64 = strings.TrimSpace(payload)
	}
	if b64 == "" {
		return "", nil, errors.New("logo data is required")
	}
	decoded, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return "", nil, errors.New("invalid base64 logo data")
	}
	if len(decoded) == 0 || len(decoded) > maxLogoBytes {
		return "", nil, errors.New("logo must be between 1 byte and 2 MiB")
	}

	sniffed := http.DetectContentType(decoded)
	switch sniffed {
	case "image/png", "image/jpeg", "image/webp":
		return sniffed, decoded, nil
	}

	if declaredMime == "image/svg+xml" {
		if !utf8.Valid(decoded) {
			return "", nil, errors.New("invalid SVG encoding")
		}
		lower := strings.ToLower(strings.TrimSpace(string(decoded)))
		// Strip optional UTF-8 BOM if present.
		lower = strings.TrimPrefix(lower, "\ufeff")
		if !(strings.HasPrefix(lower, "<svg") || (strings.HasPrefix(lower, "<?xml") && strings.Contains(lower, "<svg"))) {
			return "", nil, errors.New("invalid SVG content")
		}
		for _, bad := range []string{"<script", "javascript:", "vbscript:", "<iframe", "<object", "<embed", "<foreignobject"} {
			if strings.Contains(lower, bad) {
				return "", nil, errors.New("SVG contains disallowed active content")
			}
		}
		if svgEventAttrRe.MatchString(lower) {
			return "", nil, errors.New("SVG contains disallowed event handlers")
		}
		return "image/svg+xml", decoded, nil
	}

	return "", nil, errors.New("unsupported image format; use PNG, JPEG, WebP, or SVG")
}
