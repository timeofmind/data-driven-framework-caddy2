package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// api serves /api/v1/... — the same resources and access rules as the HTML
// pages, as JSON. Requests authenticate with the session cookie; unsafe
// methods must send the CSRF token (from GET /api/v1/me) in X-CSRF-Token.
func (a *App) api(rq *request, parts []string) {
	a.resolveSession(rq)
	a.ensureCSRF(rq)
	rq.w.Header().Set("Cache-Control", "no-store")
	if err := a.apiRoute(rq, parts); err != nil {
		a.apiError(rq, err)
	}
}

func (a *App) apiRoute(rq *request, parts []string) error {
	m := rq.r.Method
	unsafe := m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
	if unsafe && !a.checkCSRF(rq, rq.r.Header.Get("X-CSRF-Token")) {
		return errStatus(http.StatusForbidden, "missing or invalid X-CSRF-Token header")
	}
	switch {
	case len(parts) == 1 && parts[0] == "openapi.json":
		b, err := generator.OpenAPIJSON(a.Model, generator.OpenAPIOptions{Title: a.cfg.SiteName, BasePath: a.url("/api/v1")})
		if err != nil {
			return err
		}
		rq.w.Header().Set("Content-Type", "application/json")
		rq.w.Write(b)
		return nil
	case len(parts) == 1 && parts[0] == "me":
		out := map[string]any{"authenticated": rq.p != nil, "csrf_token": rq.csrf}
		if rq.p != nil {
			out["id"], out["name"], out["admin"] = rq.p.ID, rq.p.Name, rq.p.Admin
		}
		return writeJSON(rq, http.StatusOK, out)
	case len(parts) == 1 && parts[0] == "import" && a.cfg.Import.enabled():
		if m != http.MethodPost {
			return errStatus(http.StatusMethodNotAllowed, "use POST with multipart field \"files\"")
		}
		if err := a.requireLogin(rq); err != nil {
			return err
		}
		results, err := a.handleUpload(rq)
		if err != nil {
			return err
		}
		return writeJSON(rq, http.StatusOK, map[string]any{"results": results})
	case len(parts) >= 1:
		h, ok := a.bySlug[parts[0]]
		if !ok {
			return errNotFound
		}
		rq.handle = h
		if len(parts) == 1 {
			switch m {
			case http.MethodGet:
				return a.apiList(rq)
			case http.MethodPost:
				return a.apiSave(rq, nil, http.StatusCreated)
			}
			return errStatus(http.StatusMethodNotAllowed, "method not allowed")
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || len(parts) > 2 {
			return errNotFound
		}
		rq.id = id
		rec, err := a.loadReadable(rq)
		if err != nil {
			return err
		}
		switch m {
		case http.MethodGet:
			return writeJSON(rq, http.StatusOK, rec)
		case http.MethodPut, http.MethodPatch:
			if ok, _ := a.Reg.Writable(rq.ctx(), rec, rq.p); !ok {
				return errForbidden
			}
			return a.apiSave(rq, rec, http.StatusOK)
		case http.MethodDelete:
			if ok, _ := a.Reg.Writable(rq.ctx(), rec, rq.p); !ok {
				return errForbidden
			}
			if err := h.Delete(rq.ctx(), rec.ID()); err != nil {
				return errStatus(http.StatusConflict, "cannot delete: other records depend on it")
			}
			rq.w.WriteHeader(http.StatusNoContent)
			return nil
		}
		return errStatus(http.StatusMethodNotAllowed, "method not allowed")
	}
	return errNotFound
}

func (a *App) apiList(rq *request) error {
	q, _, _, err := a.listQuery(rq)
	if err != nil {
		return err
	}
	q, ok := rq.handle.Scope(q, rq.p)
	if !ok {
		if rq.p == nil {
			return errStatus(http.StatusUnauthorized, "sign in required")
		}
		return errForbidden
	}
	total, err := rq.handle.Count(rq.ctx(), q)
	if err != nil {
		return err
	}
	pageNo, _ := strconv.Atoi(rq.r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(rq.r.URL.Query().Get("per_page"))
	pageNo = max(pageNo, 1)
	if per <= 0 || per > 200 {
		per = 20
	}
	q.Limit, q.Offset = per, (pageNo-1)*per
	recs, err := rq.handle.List(rq.ctx(), q)
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []*runtime.Record{}
	}
	return writeJSON(rq, http.StatusOK, map[string]any{"data": recs, "total": total, "page": pageNo, "per_page": per})
}

func (a *App) apiSave(rq *request, rec *runtime.Record, status int) error {
	if err := a.requireLogin(rq); err != nil {
		return err
	}
	c := rq.handle.Class
	if rec == nil {
		if !runtime.CanCreate(c, rq.p) {
			return errForbidden
		}
		rec = rq.handle.New(nil)
	}
	var in map[string]any
	dec := json.NewDecoder(http.MaxBytesReader(rq.w, rq.r.Body, 1<<20))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return errStatus(http.StatusBadRequest, "body must be a JSON object")
	}
	if c.Auth != nil {
		if v, ok := in[c.Auth.Login].(string); ok {
			in[c.Auth.Login] = strings.ToLower(strings.TrimSpace(v))
		}
	}
	for k := range in {
		if s := c.Slot(k); s == nil || !s.Writable() || k == ownerOf(c) {
			delete(in, k) // read-only, unknown and owner fields are ignored, as in forms
		}
	}
	if errs := rec.Assign(in, ownerOf(c)); errs != nil {
		return &httpError{status: http.StatusUnprocessableEntity, msg: "validation failed", fields: errs}
	}
	if err := a.saveChecked(rq, rec); err != nil {
		var fe runtime.FieldErrors
		if errors.As(err, &fe) {
			return &httpError{status: http.StatusUnprocessableEntity, msg: generalError(fe), fields: fe}
		}
		return err
	}
	a.afterSave(rq, rec)
	return writeJSON(rq, status, rec)
}

func (a *App) apiError(rq *request, err error) {
	var he *httpError
	if !errors.As(err, &he) {
		if errors.Is(err, runtime.ErrNotFound) {
			he = errNotFound.(*httpError)
		} else {
			a.log.Error("api request failed", "path", rq.r.URL.Path, "error", err)
			he = &httpError{status: http.StatusInternalServerError, msg: "internal error"}
		}
	}
	body := map[string]any{"error": http.StatusText(he.status), "message": he.msg, "code": he.status}
	if len(he.fields) > 0 {
		body["fields"] = he.fields
	}
	writeJSON(rq, he.status, body)
}

func writeJSON(rq *request, status int, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(true)
	if err := enc.Encode(v); err != nil {
		return err
	}
	rq.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	rq.w.WriteHeader(status)
	rq.w.Write(buf.Bytes())
	return nil
}
