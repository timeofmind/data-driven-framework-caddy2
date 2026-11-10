package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
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
		b, err := generator.OpenAPIJSON(a.Model, generator.OpenAPIOptions{Title: a.man.Name, BasePath: a.url("/api/v1")})
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
	case len(parts) == 1 && parts[0] == "upload" && a.uploads != nil:
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
	case parts[0] == "_stats":
		if rq.p == nil || !rq.p.Admin {
			return errForbidden
		}
		return writeJSON(rq, http.StatusOK, a.stats())
	case parts[0] == "_cache" && m == http.MethodDelete:
		if rq.p == nil || !rq.p.Admin {
			return errForbidden
		}
		if a.Reg.View != nil {
			a.Reg.View.Clear()
		}
		rq.w.WriteHeader(http.StatusNoContent)
		return nil
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
			if rec.Version != 0 {
				rq.w.Header().Set("ETag", fmt.Sprintf("\"%d\"", rec.Version))
			}
			return writeJSON(rq, http.StatusOK, rec.PublicFor(rq.p))
		case http.MethodPut, http.MethodPatch:
			if ok, _ := a.Reg.Writable(rq.ctx(), rec, rq.p); !ok {
				return errForbidden
			}
			// If-Match: the version the client read; a field changed since
			// by someone else is a conflict.
			if im := strings.Trim(rq.r.Header.Get("If-Match"), "\" "); im != "" {
				if v, err := strconv.ParseInt(im, 10, 64); err == nil {
					rec.Version = v
				}
			}
			return a.apiSave(rq, rec, http.StatusOK)
		case http.MethodDelete:
			if ok, _ := a.Reg.Deletable(rq.ctx(), rec, rq.p); !ok {
				return errForbidden
			}
			if err := h.Delete(rq.ctx(), rec.ID()); err != nil {
				var keep *runtime.KeepError
				if errors.As(err, &keep) {
					return errStatus(http.StatusConflict, keep.Msg)
				}
				return errStatus(http.StatusConflict, "cannot delete: other records depend on it")
			}
			rq.w.WriteHeader(http.StatusNoContent)
			return nil
		}
		return errStatus(http.StatusMethodNotAllowed, "method not allowed")
	}
	return errNotFound
}

// loadReadable loads the request's record, answering 404 when it doesn't
// exist or the viewer may not read it (a private record's existence is not
// revealed).
func (a *App) loadReadable(rq *request) (*runtime.Record, error) {
	rec, err := rq.handle.Load(rq.ctx(), rq.id)
	if err != nil {
		return nil, err
	}
	ok, err := a.Reg.Readable(rq.ctx(), rec, rq.p)
	if err != nil && !errors.Is(err, runtime.ErrNotFound) {
		return nil, err
	}
	if !ok {
		return nil, errNotFound
	}
	a.Reg.LoadLinks(rq.ctx(), rec)
	return rec, nil
}

// apiQuery reads a list's conditions from the query string: field=value for
// enums, booleans, dates and links; q searches the title field; mine=1 the
// viewer's own; order=field [asc|desc].
func (a *App) apiQuery(rq *request) (runtime.Query, error) {
	c := rq.handle.Class
	vals := rq.r.URL.Query()
	var q runtime.Query
	for key, vs := range vals {
		s := c.Slot(key)
		if s == nil || len(vs) == 0 || vs[0] == "" {
			continue
		}
		if c.BelongsToVia(key) == nil && s.Type != "enum" && s.Type != "boolean" && s.Type != "date" {
			continue
		}
		v, err := runtime.Coerce(s, vs[0])
		if err != nil {
			continue
		}
		q.Where = append(q.Where, runtime.Cond{Field: key, Op: "=", Value: v})
	}
	if search := strings.TrimSpace(vals.Get("q")); search != "" {
		if ds := c.DisplaySlot(); ds != nil && (ds.Type == "string" || ds.Type == "text") {
			q.Where = append(q.Where, runtime.Cond{Field: ds.Name, Op: "like", Value: "%" + search + "%"})
		}
	}
	if vals.Get("mine") == "1" {
		if rq.p == nil {
			return q, errStatus(http.StatusUnauthorized, "sign in required")
		}
		if o := ownerOf(c); o != "" {
			q.Where = append(q.Where, runtime.Eq(o, rq.p.ID))
		}
	}
	if ob := vals.Get("order"); ob != "" {
		if f := strings.Fields(ob); c.Slot(f[0]) != nil {
			q.OrderBy = ob
		}
	}
	return q, nil
}

func ownerOf(c *model.Class) string {
	if c.Access == nil {
		return ""
	}
	return c.Access.Owner
}

func (a *App) apiList(rq *request) error {
	q, err := a.apiQuery(rq)
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
	data := make([]map[string]any, len(recs))
	for i, r := range recs {
		data[i] = r.PublicFor(rq.p)
	}
	return writeJSON(rq, http.StatusOK, map[string]any{"data": data, "total": total, "page": pageNo, "per_page": per})
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
		// Changing an account's password takes the current one (admins excepted).
		if pw, _ := in[c.Auth.Password].(string); pw != "" && rec.Persisted() && !rq.p.Admin {
			cur, _ := in["current_password"].(string)
			if !runtime.CheckPassword(rec, c.Auth.Password, cur) {
				return &httpError{status: http.StatusUnprocessableEntity, msg: "validation failed",
					fields: runtime.FieldErrors{"current_password": "doesn't match your current password"}}
			}
		}
	}
	role := a.Reg.WriterRole(rq.ctx(), rec, rq.p)
	// A party creating the record names its owner (access: party_creates);
	// AssignFor then takes only the owner, the party and write: party fields.
	if !a.Reg.PartyCreating(rq.ctx(), rec, rq.p, in) {
		for k := range in {
			if s := c.Slot(k); s == nil || !runtime.SlotSettable(rec, s, role) || k == ownerOf(c) {
				delete(in, k) // read-only, admin- or party-only (for others), unknown and owner fields are ignored, as in forms
			}
		}
	}
	if errs := a.Reg.AssignFor(rq.ctx(), rec, rq.p, in, ownerOf(c)); errs != nil {
		return &httpError{status: http.StatusUnprocessableEntity, msg: "validation failed", fields: errs}
	}
	if err := a.engine.Save(rq.ctx(), rq.p, rec); err != nil {
		if errors.Is(err, ux.ErrForbidden) {
			return errForbidden
		}
		var conflict *runtime.Conflict
		if errors.As(err, &conflict) {
			fields := runtime.FieldErrors{}
			for _, f := range conflict.Fields {
				fields[f] = "changed by someone else since the version you read"
			}
			return &httpError{status: http.StatusConflict, msg: "the record was changed by someone else", fields: fields}
		}
		var fe runtime.FieldErrors
		if errors.As(err, &fe) {
			return &httpError{status: http.StatusUnprocessableEntity, msg: ux.GeneralError(fe), fields: fe}
		}
		return err
	}
	return writeJSON(rq, status, rec.PublicFor(rq.p))
}

// stats is what the server knows about itself: the data view layer's
// counters and each listener.
func (a *App) stats() map[string]any {
	out := map[string]any{}
	if a.Reg.View != nil {
		out["view"] = a.Reg.View.Stats()
	}
	ls := map[string]any{}
	for name, l := range a.Reg.Listeners {
		ls[name] = map[string]any{"caps": l.Caps()}
	}
	out["listeners"] = ls
	return out
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
