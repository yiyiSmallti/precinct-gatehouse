package admin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/live"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/updater"
)

func (s *Server) getUpdater(w http.ResponseWriter, _ *http.Request, _ string) {
	if s.deps.Updater == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "updater unavailable")
		return
	}
	writeJSON(w, http.StatusOK, s.deps.Updater.Status())
}

func (s *Server) checkUpdater(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.Updater == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "updater unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	st, err := s.deps.Updater.Check(ctx)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error":   "check_failed",
			"message": err.Error(),
			"status":  st,
		})
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) applyUpdater(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.Updater == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "updater unavailable")
		return
	}
	var req struct {
		Tag   string `json:"tag"`
		Force bool   `json:"force"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	if err := s.deps.Updater.ApplyAsync(updater.ApplyOptions{
		Tag:   strings.TrimSpace(req.Tag),
		Force: req.Force,
	}); err != nil {
		if errors.Is(err, updater.ErrBusy) {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.deps.Updater.Status())
}

func (s *Server) rollbackUpdater(w http.ResponseWriter, r *http.Request, _ string) {
	if s.deps.Updater == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "updater unavailable")
		return
	}
	var req struct {
		Version string `json:"version"`
		Force   bool   `json:"force"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeJSONError(w, http.StatusBadRequest, "invalid json")
			return
		}
	}
	if err := s.deps.Updater.RollbackAsync(updater.RollbackOptions{
		TargetVersion: strings.TrimSpace(req.Version),
		Force:         req.Force,
	}); err != nil {
		if errors.Is(err, updater.ErrBusy) {
			writeJSONError(w, http.StatusConflict, err.Error())
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, s.deps.Updater.Status())
}

func validUpdaterSecretKind(kind string) bool {
	return kind == "password" || kind == "token"
}

func (s *Server) updateUpdaterSecret(w http.ResponseWriter, kind, baseHash string, applyChange func(raw *config.Config) (func(), error)) {
	res, err := s.deps.Live.ApplySerialized("web", baseHash, func(prev *live.Snapshot) ([]byte, func(), error) {
		raw := prev.Cfg.RawCopy()
		rollback, err := applyChange(raw)
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
		s.writeApplyErr(w, err)
		return
	}
	s.writeApplyOK(w, res)
}

func (s *Server) putUpdaterSecret(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Kind     string `json:"kind"`
		Value    string `json:"value"`
		BaseHash string `json:"base_hash"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if !validUpdaterSecretKind(kind) {
		writeJSONError(w, http.StatusBadRequest, "kind must be password or token")
		return
	}
	if strings.TrimSpace(req.Value) == "" {
		writeJSONError(w, http.StatusBadRequest, "value is required")
		return
	}
	settingKey := "updater_secret:" + kind
	s.updateUpdaterSecret(w, kind, req.BaseHash, func(raw *config.Config) (func(), error) {
		prevVal, prevErr := getSetting(s.deps.DB, settingKey)
		if err := setSetting(s.deps.DB, settingKey, req.Value); err != nil {
			return nil, err
		}
		if kind == "password" {
			raw.Updater.PasswordRef = "db:password"
		} else {
			raw.Updater.TokenRef = "db:token"
		}
		rollback := func() {
			if prevErr == nil {
				_ = setSetting(s.deps.DB, settingKey, prevVal)
			} else {
				_, _ = s.deps.DB.Exec(`DELETE FROM settings WHERE k=?`, settingKey)
			}
		}
		return rollback, nil
	})
}

func (s *Server) deleteUpdaterSecret(w http.ResponseWriter, r *http.Request, _ string) {
	kind := strings.TrimSpace(r.PathValue("kind"))
	if !validUpdaterSecretKind(kind) {
		writeJSONError(w, http.StatusBadRequest, "kind must be password or token")
		return
	}
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
	settingKey := "updater_secret:" + kind
	s.updateUpdaterSecret(w, kind, baseHash, func(raw *config.Config) (func(), error) {
		prevVal, prevErr := getSetting(s.deps.DB, settingKey)
		if _, err := s.deps.DB.Exec(`DELETE FROM settings WHERE k=?`, settingKey); err != nil {
			return nil, err
		}
		if kind == "password" {
			raw.Updater.PasswordRef = ""
		} else {
			raw.Updater.TokenRef = ""
		}
		rollback := func() {
			if prevErr == nil {
				_ = setSetting(s.deps.DB, settingKey, prevVal)
			}
		}
		return rollback, nil
	})
}
