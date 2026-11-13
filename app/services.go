package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// Service Configuration: the admin page every app has, where the services
// its routing names (payments, mail, a bucket, a language model) are given
// the settings and keys their listeners need. The routing file says which
// listeners an app uses and what it can say in the open (a bucket's name,
// a sender address); what it leaves out — a secret above all — an admin
// supplies here. The values live in the framework's FrameworkServiceSetting
// class (secrets sealed under the token key, seal.go), the listener is
// opened again with them at once (and at every start), and what was parked
// for it while it waited is delivered.

const servicesPath = "/admin/services"

// serviceView is one listener on the page.
type serviceView struct {
	Name, Type, Summary string
	Problem             string // why it is not open; "" when it is
	Fixed               []fixedSetting
	Fields              []ux.FormField
}

type fixedSetting struct{ Name, Value string }

type servicesPage struct {
	Message, Error string
	NoKey          bool
	Services       []serviceView
}

func (a *App) settingsClass() *runtime.Handle { return a.Reg.MustClass("FrameworkServiceSetting") }

// configurable names the routing's listeners the page offers, sorted: the
// destinations (not a source — a source's settings are the routing file's)
// whose type is public in the directory.
func (a *App) configurable() []string {
	rt := a.Reg.Routing
	sources := map[string]bool{}
	for _, r := range rt.Routes {
		sources[r.Source] = true
	}
	infos := infosByType()
	var out []string
	for name, cfg := range rt.Listeners {
		if sources[name] {
			continue
		}
		if info, ok := infos[cfg.Type]; !ok || info.Private {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func infosByType() map[string]listener.Info {
	out := map[string]listener.Info{}
	for _, i := range listener.Infos(false) {
		out[i.Type] = i
	}
	return out
}

// storedSettings reads what admins stored for every listener, unsealed, and
// which of them are secrets. A table not there yet (a database before its
// first Prepare) reads as nothing stored.
func storedSettings(ctx context.Context, reg *runtime.Registry, tokenKey []byte, log *slog.Logger) (map[string]map[string]string, map[string]map[string]bool) {
	values := map[string]map[string]string{}
	secrets := map[string]map[string]bool{}
	h, err := reg.Class("FrameworkServiceSetting")
	if err != nil {
		return values, secrets
	}
	recs, err := h.List(ctx, runtime.Query{Limit: 10000})
	if err != nil {
		return values, secrets
	}
	for _, r := range recs {
		ln, st := r.String("listener"), r.String("setting")
		if values[ln] == nil {
			values[ln] = map[string]string{}
			secrets[ln] = map[string]bool{}
		}
		v, err := unseal(tokenKey, r.String("value"))
		if err != nil {
			log.Error("a stored service setting cannot be read", "listener", ln, "setting", st, "error", err)
			continue
		}
		values[ln][st] = v
		if r.Bool("secret") {
			secrets[ln][st] = true
		}
	}
	return values, secrets
}

// reopenStored opens again, at the start, every destination that has
// settings stored for it — before the app's hooks are attached, so a
// listener that only opens with its stored key is the one they go on.
func reopenStored(ctx context.Context, reg *runtime.Registry, tokenKey []byte, log *slog.Logger) {
	values, _ := storedSettings(ctx, reg, tokenKey, log)
	for name, s := range values {
		if _, ok := reg.Routing.Listeners[name]; !ok {
			continue
		}
		if err := reg.Reopen(ctx, name, s, nil); err != nil {
			log.Warn("a destination does not open with its stored settings; what flows to it is parked", "listener", name, "error", err)
		}
	}
}

// services serves the page and its saves. Admins only.
func (a *App) services(rq *request) error {
	if rq.p == nil {
		return errStatus(http.StatusUnauthorized, "Please sign in first")
	}
	if !rq.p.Admin {
		return errForbidden
	}
	pg := servicesPage{NoKey: len(a.cfg.TokenKey) == 0}
	if rq.r.Method == http.MethodPost {
		name := rq.r.PostFormValue("listener")
		if err := a.saveService(rq, name); err != nil {
			var he *httpError
			if errors.As(err, &he) {
				return err
			}
			pg.Error = fmt.Sprintf("%s: %v", name, err)
		} else {
			http.Redirect(rq.w, rq.r, a.url(servicesPath)+"?saved="+url.QueryEscape(name)+"#svc-"+url.PathEscape(name), http.StatusSeeOther)
			return nil
		}
	} else if saved := rq.r.URL.Query().Get("saved"); saved != "" {
		if err := a.Reg.Unconfigured(saved); err != nil {
			pg.Message = fmt.Sprintf("Saved %s. It is still not configured: %v", saved, err)
		} else {
			pg.Message = fmt.Sprintf("Saved %s: configured and running.", saved)
		}
	}
	pg.Services = a.serviceViews(rq.ctx())
	a.site.Render(a.webRequest(rq), "services", "Service Configuration", pg)
	return nil
}

// serviceViews builds the page's sections from the routing, the directory
// and what is stored.
func (a *App) serviceViews(ctx context.Context) []serviceView {
	infos := infosByType()
	values, secrets := storedSettings(ctx, a.Reg, a.cfg.TokenKey, a.log)
	var out []serviceView
	for _, name := range a.configurable() {
		cfg := a.Reg.Routing.Listeners[name]
		info := infos[cfg.Type]
		sv := serviceView{Name: name, Type: cfg.Type, Summary: info.Summary}
		if err := a.Reg.Unconfigured(name); err != nil {
			sv.Problem = err.Error()
		}
		secretOf := map[string]bool{}
		for _, s := range info.Settings {
			secretOf[s.Name] = s.Secret
		}
		var fixed []string
		for k := range cfg.Settings {
			fixed = append(fixed, k)
		}
		sort.Strings(fixed)
		for _, k := range fixed {
			v := cfg.Settings[k]
			if secretOf[k] || strings.Contains(k, "key") || strings.Contains(k, "secret") || strings.Contains(k, "password") || k == "dsn" {
				v = "(set)"
			}
			sv.Fixed = append(sv.Fixed, fixedSetting{Name: k, Value: v})
		}
		for _, s := range info.Settings {
			if _, inRouting := cfg.Settings[s.Name]; inRouting {
				continue
			}
			help := s.Doc
			if s.Default != "" {
				help = strings.TrimRight(help, ".") + ". Default: " + s.Default + "."
			}
			f := ux.FormField{Name: "s_" + s.Name, Label: s.Name, Type: "text", Required: s.Required, Help: help, Placeholder: s.Default}
			stored, isStored := values[name][s.Name]
			if s.Secret {
				f.Type = "password"
				f.Required = false // blank keeps what is stored
				if isStored && secrets[name][s.Name] {
					f.Help = "Stored. Leave blank to keep it. " + help
					f.Placeholder = "••••••••"
				} else if s.Required {
					f.Help = "Required. " + help
				}
			} else if isStored {
				f.Value = stored
			}
			sv.Fields = append(sv.Fields, f)
			if s.Secret && isStored {
				sv.Fields = append(sv.Fields, ux.FormField{Name: "clear_" + s.Name, Label: "Forget the stored " + s.Name, Type: "checkbox"})
			}
		}
		out = append(out, sv)
	}
	return out
}

// saveService stores a listener's posted settings and opens it again with
// them. An error that is not an httpError is shown on the page.
func (a *App) saveService(rq *request, name string) error {
	infos := infosByType()
	cfg, ok := a.Reg.Routing.Listeners[name]
	if !ok {
		return errStatus(http.StatusBadRequest, "No such service")
	}
	found := false
	for _, n := range a.configurable() {
		found = found || n == name
	}
	if !found {
		return errStatus(http.StatusBadRequest, "That service is not configured on this page")
	}
	info := infos[cfg.Type]
	f := rq.r.PostForm
	ctx := rq.ctx()
	for _, s := range info.Settings {
		if _, inRouting := cfg.Settings[s.Name]; inRouting {
			continue
		}
		v := strings.TrimSpace(f.Get("s_" + s.Name))
		switch {
		case s.Secret && f.Get("clear_"+s.Name) == "1":
			if err := a.forgetSetting(ctx, name, s.Name); err != nil {
				return err
			}
		case s.Secret && v == "":
			// blank keeps what is stored
		case v == "":
			if err := a.forgetSetting(ctx, name, s.Name); err != nil {
				return err
			}
		default:
			if err := a.storeSetting(ctx, rq.p.ID, name, s.Name, v, s.Secret); err != nil {
				return err
			}
		}
	}
	// The stored settings are the truth: the destination opens with them,
	// or is left waiting with the reason, which the page shows.
	values, _ := storedSettings(ctx, a.Reg, a.cfg.TokenKey, a.log)
	if err := a.Reg.Reopen(ctx, name, values[name], a.readyListener); err != nil {
		a.log.Warn("a service does not open with its settings", "listener", name, "by", rq.p.ID, "error", err)
		return nil
	}
	a.log.Info("a service was configured", "listener", name, "by", rq.p.ID)
	return nil
}

func (a *App) storeSetting(ctx context.Context, by int64, name, setting, value string, secret bool) error {
	h := a.settingsClass()
	if secret {
		sealed, err := seal(a.cfg.TokenKey, value)
		if err != nil {
			return fmt.Errorf("%s cannot be kept: %w (the server needs a token_key_file)", setting, err)
		}
		value = sealed
	}
	rec, err := h.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", name), runtime.Eq("setting", setting)}})
	if errors.Is(err, runtime.ErrNotFound) {
		rec = h.New(map[string]any{"listener": name, "setting": setting})
	} else if err != nil {
		return err
	}
	rec.Values["value"] = value
	rec.Values["secret"] = secret
	rec.Values["updated_by"] = by
	rec.Values["updated_at"] = time.Now().UTC().Format(timeLayout)
	return h.Save(ctx, rec)
}

func (a *App) forgetSetting(ctx context.Context, name, setting string) error {
	h := a.settingsClass()
	rec, err := h.First(ctx, runtime.Query{Where: []runtime.Cond{runtime.Eq("listener", name), runtime.Eq("setting", setting)}})
	if errors.Is(err, runtime.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return h.Delete(ctx, rec.ID())
}
