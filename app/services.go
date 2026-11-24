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
	"github.com/timeofmind/data-driven-framework-go/listener/signin"
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
	Fields              []ux.FormField // the mandatory settings, shown
	Advanced            []ux.FormField // the others, folded under "Advanced"
	AdvancedOpen        bool           // one of them is set: the fold starts open
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
	Currencies          string // a payment processor's currencies, for the card
	Countries           string // the countries its accounts may be in (Payment Checkout)
	Unknown             bool   // a kind the listener type does not offer
	Use                 bool
	Rank                int
	Fixed               []fixedSetting
	Fields              []ux.FormField
	Advanced            []ux.FormField
	AdvancedOpen        bool
}

type fixedSetting struct{ Name, Value string }

// currenciesText is a provider's currencies as the card shows them: the
// codes, then the note; "" when the directory says nothing.
func currenciesText(p *listener.Provider) string {
	s := strings.Join(p.Currencies, ", ")
	if p.CurrenciesNote != "" {
		if s != "" {
			s += " — "
		}
		s += p.CurrenciesNote
	}
	if s == "" {
		return ""
	}
	return "Currencies: " + s
}

type servicesPage struct {
	Message, Error   string
	NoKey            bool
	Services         []serviceView
	Checkout         string // Payment Checkout, where the payment (and shipping) services are
	CheckoutShipping bool
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
		if info, ok := infos[cfg.Type]; !ok || info.Private || cfg.Type == signin.Type { // signin has its own page
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
				pv.Summary, pv.Currencies = np.Provider.Summary, currenciesText(np.Provider)
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
		pv := providerView{Name: p.Type, Type: p.Type, Summary: p.Summary, Currencies: currenciesText(&p)}
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
	// From the database as it is now: another node may have saved them.
	recs, err := h.ListSource(ctx, runtime.Query{Limit: 10000})
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

// settingsEvery is how often a node reads the stored settings again, for
// those an admin saved on another node of the app.
var settingsEvery = 30 * time.Second

// watchSettings keeps the node's destinations opened with the settings
// stored: a save on the Service Configuration page reopens a destination on
// the node that served it, and the app's other nodes (and a preview sharing
// its database) learn of it here.
func (a *App) watchSettings() {
	t := time.NewTicker(settingsEvery)
	defer t.Stop()
	for {
		select {
		case <-a.bg.Done():
			return
		case <-t.C:
			a.syncSettings(a.bg)
		}
	}
}

// syncSettings opens again every destination whose stored settings differ
// from those it was last opened with. A setting that cannot be read was
// logged at the start, and is not logged again on every pass.
func (a *App) syncSettings(ctx context.Context) {
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	values, _ := storedSettings(ctx, a.Reg, a.cfg.TokenKey, slog.New(slog.DiscardHandler))
	a.setCheckoutValues(values[checkoutSettings])
	for name, cfg := range a.Reg.Routing.Listeners {
		if _, ok := a.Reg.Listeners[name].(*listener.Switch); !ok {
			continue
		}
		if sameSettings(a.Reg.Settings(name), withStored(cfg.Settings, values[name])) {
			continue
		}
		if err := a.Reg.Reopen(ctx, name, values[name], a.readyListener); err != nil {
			a.log.Warn("a service's settings changed on another node; it does not open with them", "listener", name, "error", err)
			continue
		}
		a.log.Info("a service's settings changed on another node; opened with them", "listener", name)
	}
}

// withStored is the routing's settings with the stored ones filling what
// the routing does not give (as the registry merges them).
func withStored(routed, stored map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range stored {
		if v != "" {
			out[k] = v
		}
	}
	for k, v := range routed {
		out[k] = v
	}
	return out
}

func sameSettings(x, y map[string]string) bool {
	if len(x) != len(y) {
		return false
	}
	for k, v := range x {
		if w, ok := y[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// services serves the page and its saves: admins only, with the authority
// the section asks for.
func (a *App) services(rq *request) error {
	if ok, err := a.adminSection(rq, servicesPath); !ok {
		return err
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
	} else {
		// what another node saved, before this one shows its state
		a.syncSettings(rq.ctx())
	}
	if saved := rq.r.URL.Query().Get("saved"); rq.r.Method != http.MethodPost && saved != "" {
		if err := a.Reg.Unconfigured(saved); err != nil {
			pg.Message = fmt.Sprintf("Saved %s. It is still not configured: %v", saved, err)
		} else {
			pg.Message = fmt.Sprintf("Saved %s: configured and running.", saved)
		}
	}
	pay, ship := a.commerceListeners()
	held := map[string]bool{}
	for _, n := range append(append([]string(nil), pay...), ship...) {
		held[n] = true
	}
	for _, sv := range a.serviceViews(rq.ctx()) {
		if len(pay) > 0 && held[sv.Name] {
			continue // on Payment Checkout
		}
		pg.Services = append(pg.Services, sv)
	}
	if len(pay) > 0 {
		pg.Checkout, pg.CheckoutShipping = a.url(a.checkoutPath), len(ship) > 0
	}
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
		sv.Fields, sv.Advanced, sv.AdvancedOpen = fieldsFor(own, cfg.Settings, values[name], secrets[name])
		for i := range providers {
			providers[i].Fields, providers[i].Advanced, providers[i].AdvancedOpen = fieldsFor(providerSettings(info, providers[i].Type, providers[i].Name), cfg.Settings, values[name], secrets[name])
			if derived {
				optionalUnlessUsed(providers[i].Fields, providers[i].Name)
			}
		}
		if derived {
			// The ones in use first, in their order; then the rest as the
			// directory lists them — a long list reads from what matters.
			sort.SliceStable(providers, func(i, j int) bool {
				if providers[i].Use != providers[j].Use {
					return providers[i].Use
				}
				return providers[i].Use && providers[i].Rank < providers[j].Rank
			})
		}
		sv.Providers = providers
		out = append(out, sv)
	}
	return out
}

// fieldsFor makes the form fields of settings not given by the routing
// file: a stored plain value shown, a stored secret never — blank keeps it,
// a checkbox forgets it.
//
// The mandatory settings come first; the others (a model preference, a timeout,
// a bucket the app may do without) are returned apart, for the page to fold
// under "Advanced" so a first look shows only what must be given. open says
// one of the others holds a value, so the fold is not closed over it.
func fieldsFor(settings []listener.Setting, inRouting map[string]string, values map[string]string, secrets map[string]bool) (main, advanced []ux.FormField, open bool) {
	current := func(name string) string { // a setting's value as the page starts: stored, routed, else its default
		if v, ok := inRouting[name]; ok {
			return v
		}
		if v := values[name]; v != "" {
			return v
		}
		for _, o := range settings {
			if o.Name == name {
				if o.Default != "" {
					return o.Default
				}
				if len(o.Choices) > 0 {
					return o.Choices[0].Value
				}
			}
		}
		return ""
	}
	for _, s := range settings {
		out := &main
		if !s.Required {
			out = &advanced
		}
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
		if len(s.Choices) > 0 {
			// one of a few: a select, and the fields of the other choices hidden
			f.Type, f.Placeholder = "select", ""
			cur := current(s.Name)
			for _, c := range s.Choices {
				f.Options = append(f.Options, ux.Option{Value: c.Value, Label: c.Label, Selected: c.Value == cur})
			}
		}
		if name, vals, ok := strings.Cut(s.ShownWhen, "="); ok {
			f.ShowIf = "s_" + name + "=" + vals
			f.Hidden = true
			for _, v := range strings.Split(vals, ",") {
				if strings.TrimSpace(v) == current(name) {
					f.Hidden = false
				}
			}
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
		if !s.Required && isStored && (stored != "" || secrets[s.Name]) && !(len(s.Choices) > 0 && stored == current(s.Name) && stored == s.Default) {
			open = true
		}
		*out = append(*out, f)
		if s.Secret && isStored {
			cl := ux.FormField{Name: "clear_" + s.Name, Label: "Forget the stored " + s.Name, Type: "checkbox", ShowIf: f.ShowIf, Hidden: f.Hidden}
			*out = append(*out, cl)
		}
	}
	return main, advanced, open
}

// optionalUnlessUsed: a provider the admin may leave unticked asks for
// nothing in the browser — its mandatory settings are mandatory only for
// using it, and a form must save with a provider left empty (a browser would
// refuse the whole form for a field of a provider not in use, hidden).
// The listener says what a ticked provider lacks when it opens.
func optionalUnlessUsed(fields []ux.FormField, provider string) {
	for i := range fields {
		f := &fields[i]
		if f.Type == "checkbox" {
			continue
		}
		f.Help = "Required to use " + provider + ". " + strings.TrimPrefix(f.Help, "Required. ")
		f.Required = false
	}
}

// saveService stores a listener's posted settings and opens it again with
// them. An error that is not an httpError is shown on the page.
func (a *App) saveService(rq *request, name string) error { return a.saveServiceExcept(rq, name, nil) }

// saveServiceExcept saves a service's card, leaving the settings in except
// as they are (another part of the page holds them: Payment Checkout's
// store keeps the payment listener's currency).
func (a *App) saveServiceExcept(rq *request, name string, except map[string]bool) error {
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
	// What this save changed: a stored write reaches the database through the
	// change log, so a read of it straight after may not see it yet; the
	// reopen below takes these over what it reads (nil: forgotten).
	changed := map[string]*string{}
	keep := func(setting, v string) { changed[setting] = &v }
	for _, s := range settings {
		if _, inRouting := cfg.Settings[s.Name]; inRouting || except[s.Name] {
			continue
		}
		v := strings.TrimSpace(f.Get("s_" + s.Name))
		switch {
		case s.Secret && f.Get("clear_"+s.Name) == "1":
			if err := a.forgetSetting(ctx, name, s.Name); err != nil {
				return err
			}
			changed[s.Name] = nil
		case s.Secret && v == "":
			// blank keeps what is stored
		case v == "":
			if err := a.forgetSetting(ctx, name, s.Name); err != nil {
				return err
			}
			changed[s.Name] = nil
		default:
			if err := a.storeSetting(ctx, rq.p.ID, name, s.Name, v, s.Secret); err != nil {
				return err
			}
			keep(s.Name, v)
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
			changed["providers"] = nil
		} else if err := a.storeSetting(ctx, rq.p.ID, name, "providers", strings.Join(names, ", "), false); err != nil {
			return err
		} else {
			keep("providers", strings.Join(names, ", "))
		}
	}
	// The stored settings are the truth: the destination opens with them,
	// or is left waiting with the reason, which the page shows.
	values, _ := storedSettings(ctx, a.Reg, a.cfg.TokenKey, a.log)
	if err := a.Reg.Reopen(ctx, name, withChanges(values[name], changed), a.readyListener); err != nil {
		a.log.Warn("a service does not open with its settings", "listener", name, "by", rq.p.ID, "error", err)
		return nil
	}
	a.log.Info("a service was configured", "listener", name, "by", rq.p.ID)
	return nil
}

// withChanges is the stored settings read back with a save's own changes
// over them: what it stored, and nothing of what it forgot.
func withChanges(stored map[string]string, changed map[string]*string) map[string]string {
	out := map[string]string{}
	for k, v := range stored {
		out[k] = v
	}
	for k, v := range changed {
		if v == nil {
			delete(out, k)
		} else {
			out[k] = *v
		}
	}
	return out
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
