package admin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/tiered"
)

func (s *Server) tieredRouter(name string) (*tiered.Router, bool) {
	if s.deps.Tiered == nil || name == "" {
		return nil, false
	}
	return s.deps.Tiered.Router(name)
}

func (s *Server) getTiered(w http.ResponseWriter, _ *http.Request, _ string) {
	providers := []tiered.ProviderStatus{}
	if s.deps.Tiered != nil {
		providers = s.deps.Tiered.Statuses()
		if providers == nil {
			providers = []tiered.ProviderStatus{}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (s *Server) getTieredStarters(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("provider")
	rt, ok := s.tieredRouter(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "tiered provider not found")
		return
	}
	starters := rt.Starters()
	if starters == nil {
		starters = []tiered.StarterView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": name,
		"starters": starters,
	})
}

func (s *Server) pinTieredStarter(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("provider")
	rt, ok := s.tieredRouter(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "tiered provider not found")
		return
	}
	var req struct {
		Hash   string `json:"hash"`
		Pinned bool   `json:"pinned"`
		Label  string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.Hash == "" {
		writeJSONError(w, http.StatusBadRequest, "hash is required")
		return
	}
	view, err := rt.PinStarter(req.Hash, req.Pinned, req.Label)
	if err != nil {
		if errors.Is(err, tiered.ErrStarterNotFound) {
			writeJSONError(w, http.StatusNotFound, "starter not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"starter": view})
}

func (s *Server) deleteTieredStarter(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("provider")
	rt, ok := s.tieredRouter(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "tiered provider not found")
		return
	}
	hash := r.PathValue("hash")
	deleted, err := rt.DeleteStarter(hash)
	if err != nil {
		if errors.Is(err, tiered.ErrStarterNotFound) {
			writeJSONError(w, http.StatusNotFound, "starter not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) warmTieredStarters(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("provider")
	rt, ok := s.tieredRouter(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "tiered provider not found")
		return
	}
	var req struct {
		Hash  string `json:"hash"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid json")
		return
	}
	results, err := rt.WarmStarter(req.Hash, req.Force)
	if err != nil {
		if errors.Is(err, tiered.ErrShadowMode) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":   "shadow_mode",
				"message": "当前处于 shadow 观察模式，未触发后端 KV cache 预热；切换到 active 模式后再试",
			})
			return
		}
		if errors.Is(err, tiered.ErrStarterNotFound) {
			writeJSONError(w, http.StatusNotFound, "starter not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if results == nil {
		results = []tiered.WarmResult{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (s *Server) resetTieredShadow(w http.ResponseWriter, r *http.Request, _ string) {
	name := r.PathValue("provider")
	rt, ok := s.tieredRouter(name)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "tiered provider not found")
		return
	}
	rt.ResetShadow()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
