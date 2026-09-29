package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Sirkolko/terraform-recovery/internal/discovery"
	"github.com/Sirkolko/terraform-recovery/internal/recovery"
)

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /static/{file}", s.staticFile)

	mux.HandleFunc("GET /api/session", s.session)
	mux.HandleFunc("GET /api/resource", s.resource)
	mux.HandleFunc("GET /api/cloud", s.cloud)
	mux.HandleFunc("GET /api/pair", s.pair)
	mux.HandleFunc("GET /api/profiles", s.profiles)
	mux.HandleFunc("GET /api/imports", s.imports)
	mux.HandleFunc("GET /api/download/imports.tf", s.downloadImports)
	mux.HandleFunc("GET /api/download/mapping.json", s.downloadMapping)
	mux.HandleFunc("GET /api/plan/text", s.planText)
	mux.HandleFunc("GET /api/jobs/{id}", s.job)

	mux.HandleFunc("POST /api/identity", s.identity)
	mux.HandleFunc("POST /api/scan", s.scan)
	mux.HandleFunc("POST /api/link", s.link)
	mux.HandleFunc("POST /api/accept", s.accept)
	mux.HandleFunc("POST /api/accept-above", s.acceptAbove)
	mux.HandleFunc("POST /api/unlink", s.unlink)
	mux.HandleFunc("POST /api/manual-id", s.manualID)
	mux.HandleFunc("POST /api/ignore", s.ignore)
	mux.HandleFunc("POST /api/unignore", s.unignore)
	mux.HandleFunc("POST /api/instance-keys", s.instanceKeys)
	mux.HandleFunc("POST /api/reload", s.reload)
	mux.HandleFunc("POST /api/modules", s.modules)
	mux.HandleFunc("POST /api/plan", s.plan)
	mux.HandleFunc("POST /api/apply", s.apply)
	mux.HandleFunc("POST /api/remove-recovery-file", s.removeRecoveryFile)
	mux.HandleFunc("POST /api/jobs/{id}/cancel", s.cancelJob)
	return s.secure(mux)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, recovery.ErrUnknownAddress), errors.Is(err, recovery.ErrUnknownCloud):
		status = http.StatusNotFound
	case errors.Is(err, recovery.ErrBusy):
		status = http.StatusConflict
	}
	writeError(w, status, err.Error())
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return v, false
	}
	return v, true
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	v := s.svc.View()
	writeJSON(w, http.StatusOK, map[string]any{"version": s.version, "session": v})
}

func (s *Server) resource(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.Detail(r.URL.Query().Get("address"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) cloud(w http.ResponseWriter, r *http.Request) {
	d, err := s.svc.Cloud(r.URL.Query().Get("key"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c, err := s.svc.Pair(q.Get("address"), q.Get("cloud_key"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) profiles(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"profiles": discovery.Profiles(),
		"default":  discovery.DefaultProfile(),
	})
}

func (s *Server) identity(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Profile string `json:"profile"`
	}](w, r)
	if !ok {
		return
	}
	id, err := s.svc.Identity(r.Context(), req.Profile)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, id)
}

func (s *Server) startedJob(w http.ResponseWriter, job *recovery.Job, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"job": job.ID})
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Profile string   `json:"profile"`
		Regions []string `json:"regions"`
	}](w, r)
	if !ok {
		return
	}
	job, err := s.svc.StartScan(req.Profile, req.Regions)
	s.startedJob(w, job, err)
}

func (s *Server) job(w http.ResponseWriter, r *http.Request) {
	job, ok := s.svc.Job(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "unknown job")
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	v := job.View(offset)
	v.Result = nil // results are read through the session
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.CancelJob(r.PathValue("id")); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) done(w http.ResponseWriter, err error) {
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) link(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Address  string `json:"address"`
		CloudKey string `json:"cloud_key"`
	}](w, r)
	if ok {
		s.done(w, s.svc.Link(req.Address, req.CloudKey))
	}
}

func (s *Server) accept(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Addresses []string `json:"addresses"`
	}](w, r)
	if !ok {
		return
	}
	n, err := s.svc.Accept(req.Addresses)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": n})
}

func (s *Server) acceptAbove(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Threshold int `json:"threshold"`
	}](w, r)
	if !ok {
		return
	}
	n, err := s.svc.AcceptAbove(req.Threshold)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"accepted": n})
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Address string `json:"address"`
	}](w, r)
	if ok {
		s.done(w, s.svc.Unlink(req.Address))
	}
}

func (s *Server) manualID(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Address  string `json:"address"`
		ImportID string `json:"import_id"`
	}](w, r)
	if ok {
		s.done(w, s.svc.SetManualID(req.Address, req.ImportID))
	}
}

func (s *Server) ignore(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Kind   string `json:"kind"`
		Key    string `json:"key"`
		Reason string `json:"reason"`
	}](w, r)
	if ok {
		s.done(w, s.svc.Ignore(req.Kind, req.Key, req.Reason))
	}
}

func (s *Server) unignore(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Kind string `json:"kind"`
		Key  string `json:"key"`
	}](w, r)
	if ok {
		s.done(w, s.svc.Unignore(req.Kind, req.Key))
	}
}

func (s *Server) instanceKeys(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		Resource string   `json:"resource"`
		Keys     []string `json:"keys"`
	}](w, r)
	if ok {
		s.done(w, s.svc.SetInstanceKeys(req.Resource, req.Keys))
	}
}

func (s *Server) reload(w http.ResponseWriter, r *http.Request) {
	if _, ok := decode[struct{}](w, r); ok {
		s.done(w, s.svc.Reload())
	}
}

func (s *Server) modules(w http.ResponseWriter, r *http.Request) {
	if _, ok := decode[struct{}](w, r); ok {
		job, err := s.svc.StartModuleInstall()
		s.startedJob(w, job, err)
	}
}

func (s *Server) imports(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Preview()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) downloadImports(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Preview()
	if err != nil {
		s.fail(w, err)
		return
	}
	if p.HCL == "" {
		writeError(w, http.StatusNotFound, "no confirmed mappings to import yet")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="recovery.import.tf"`)
	w.Write([]byte(p.HCL))
}

func (s *Server) downloadMapping(w http.ResponseWriter, r *http.Request) {
	data, err := s.svc.MappingJSON()
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mapping.json"`)
	w.Write(data)
}

func (s *Server) plan(w http.ResponseWriter, r *http.Request) {
	if _, ok := decode[struct{}](w, r); ok {
		job, err := s.svc.StartPlan()
		s.startedJob(w, job, err)
	}
}

func (s *Server) planText(w http.ResponseWriter, r *http.Request) {
	text, err := s.svc.PlanText(r.URL.Query().Get("id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func (s *Server) apply(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[struct {
		PlanID  string `json:"plan_id"`
		Confirm bool   `json:"confirm"`
	}](w, r)
	if !ok {
		return
	}
	job, err := s.svc.StartApply(req.PlanID, req.Confirm)
	s.startedJob(w, job, err)
}

func (s *Server) removeRecoveryFile(w http.ResponseWriter, r *http.Request) {
	if _, ok := decode[struct{}](w, r); !ok {
		return
	}
	removed, err := s.svc.RemoveRecoveryFile()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"removed": removed})
}
