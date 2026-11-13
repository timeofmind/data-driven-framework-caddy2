package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
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

// serviceView is one listener on the page: its own settings, and — for a
// listener type working through providers (a payment listener's
// processors) — a card per provider.
type serviceView struct {
	Name, Type, Summary string
	Problem             string // why it is not open; "" when it is
	Fixed               []fixedSetting
	Fields              []ux.FormField
	Providers           []providerView
	// ProvidersFixed: the routing file names the providers in use; the
	// cards are those. Otherwise the cards are every kind the type offers,
	// and the admin ticks the ones in use.
	ProvidersFixed bool
}

// providerView is one provider's card: its name (the prefix of its
// settings), its kind, whether it is in use and in what order.
type providerView struct {
	Name, Type, Summary string
	Unknown             bool // a kind the listener type does not offer
	Use                 bool
	Rank                int
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

// providersOf lists a listener's provider cards: the providers the routing
// file names (each of its kind), or, when it names none, every kind the
// type offers under its own name — in use when the (stored) providers
// setting lists it, ranked by its place there.
func providersOf(info listener.Info, cfg listener.Config, settings map[string]string) ([]providerView, bool) {
	if len(info.Providers) == 0 {
		return nil, false
	}
	if _, fixed := cfg.Settings["providers"]; fixed {
		var out []providerView
		for _, np := range info.ProvidersOf(settings) {
			pv := providerView{Name: np.Name, Type: np.Type, Use: true}
			if np.Provider != nil {
				pv.Summary = np.Provider.Summary
			} else {
				pv.Unknown = true
			}
			out = append(out, pv)
		}
		return out, true
	}
	inUse := map[string]int{}
	for i, n := range listener.ProviderNames(settings["providers"]) {
		inUse[n] = i + 1
	}
	var out []providerView
	for _, p := range info.Providers {
		pv := providerView{Name: p.Type, Type: p.Type, Summary: p.Summary}
		if r, ok := inUse[p.Type]; ok {
			pv.Use, pv.Rank = true, r
		}
		out = append(out, pv)
	}
	return out, false
}

// providerSettings are a provider's settings under its name: stripe's
// secret_key is the listener's stripe_secret_key.
func providerSettings(info listener.Info, kind, name string) []listener.Setting {
	p := info.Provider(kind)
	if p == nil {
		return nil
	}
	out := make([]listener.Setting, 0, len(p.Settings))
	for _, s := range p.Settings {
		s.Name = name + "_" + s.Name
		out = append(out, s)
	}
	return out
}

// listenerSettings are the listener-wide settings offered as fields: all
// of the type's own, less providers when the cards decide it.
func listenerSettings(info listener.Info, derived bool) []listener.Setting {
	if !derived {
		return info.Settings
	}
	var out []listener.Setting
	for _, s := range info.Settings {
		if s.Name != "providers" {
			out = append(out, s)
		}
	}
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
		merged := a.Reg.Settings(name)
		providers, fixed := providersOf(info, cfg, merged)
		sv.ProvidersFixed = fixed
		derived := len(info.Providers) > 0 && !fixed
		own := listenerSettings(info, derived)
		known := map[string]bool{}
		for _, s := range own {
			known[s.Name] = true
		}
		for i := range providers {
			for _, s := range providerSettings(info, providers[i].Type, providers[i].Name) {
				known[s.Name] = true
			}
		}
		// What the routing file gives, shown fixed: with the listener, or
		// on its provider's card.
		var fixedNames []string
		for k := range cfg.Settings {
			fixedNames = append(fixedNames, k)
		}
		sort.Strings(fixedNames)
		for _, k := range fixedNames {
			v := cfg.Settings[k]
			if strings.Contains(k, "key") || strings.Contains(k, "secret") || strings.Contains(k, "password") || strings.Contains(k, "token") || k == "dsn" {
				v = "(set)"
			}
			if derived && k == "providers" {
				continue
			}
			placed := false
			for i := range providers {
				if strings.HasPrefix(k, providers[i].Name+"_") {
					providers[i].Fixed = append(providers[i].Fixed, fixedSetting{Name: k, Value: v})
					placed = true
					break
				}
			}
			if !placed {
				sv.Fixed = append(sv.Fixed, fixedSetting{Name: k, Value: v})
			}
		}
		sv.Fields = fieldsFor(own, cfg.Settings, values[name], secrets[name])
		for i := range providers {
			providers[i].Fields = fieldsFor(providerSettings(info, providers[i].Type, providers[i].Name), cfg.Settings, values[name], secrets[name])
		}
		sv.Providers = providers
		out = append(out, sv)
	}
	return out
}

// fieldsFor makes the form fields of settings not given by the routing
// file: a stored plain value shown, a stored secret never — blank keeps it,
// a checkbox forgets it.
func fieldsFor(settings []listener.Setting, inRouting map[string]string, values map[string]string, secrets map[string]bool) []ux.FormField {
	var out []ux.FormField
	for _, s := range settings {
		if _, fixed := inRouting[s.Name]; fixed {
			continue
		}
		help := s.Doc
		if s.Default != "" {
			help = strings.TrimRight(help, ".") + ". Default: " + s.Default + "."
		}
		f := ux.FormField{Name: "s_" + s.Name, Label: s.Name, Type: "text", Required: s.Required, Help: help, Placeholder: s.Default}
		if s.Multiline {
			f.Type = "textarea"
		}
		stored, isStored := values[s.Name]
		if s.Secret {
			if !s.Multiline {
				f.Type = "password"
			}
			f.Required = false // blank keeps what is stored
			if isStored && secrets[s.Name] {
				f.Help = "Stored. Leave blank to keep it. " + help
				f.Placeholder = "••••••••"
			} else if s.Required {
				f.Help = "Required. " + help
			}
		} else if isStored {
			f.Value = stored
		}
		out = append(out, f)
		if s.Secret && isStored {
			out = append(out, ux.FormField{Name: "clear_" + s.Name, Label: "Forget the stored " + s.Name, Type: "checkbox"})
		}
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
	merged := a.Reg.Settings(name)
	providers, fixed := providersOf(info, cfg, merged)
	derived := len(info.Providers) > 0 && !fixed
	settings := listenerSettings(info, derived)
	for i := range providers {
		settings = append(settings, providerSettings(info, providers[i].Type, providers[i].Name)...)
	}
	for _, s := range settings {
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
	if derived {
		// The providers in use are the cards ticked, in the order given
		// (then the directory's).
		type pick struct {
			name string
			rank int
			i    int
		}
		var picks []pick
		for i, p := range providers {
			if f.Get("use_"+p.Name) != "1" {
				continue
			}
			r, _ := strconv.Atoi(strings.TrimSpace(f.Get("rank_" + p.Name)))
			if r <= 0 {
				r = 1000 + i
			}
			picks = append(picks, pick{p.Name, r, i})
		}
		sort.SliceStable(picks, func(i, j int) bool { return picks[i].rank < picks[j].rank })
		var names []string
		for _, p := range picks {
			names = append(names, p.name)
		}
		if len(names) == 0 {
			if err := a.forgetSetting(ctx, name, "providers"); err != nil {
				return err
			}
		} else if err := a.storeSetting(ctx, rq.p.ID, name, "providers", strings.Join(names, ", "), false); err != nil {
			return err
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
