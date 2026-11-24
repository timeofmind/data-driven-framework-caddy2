package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/payment"
	"github.com/timeofmind/data-driven-framework-go/shop"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// Payment Checkout (the framework's docs/commerce.md, "Payment Checkout"):
// the admin page of an app that takes payments, in the order an owner sets
// it up — the payment processors (their cards, moved here from Service
// Configuration, each checked with its processor), the store (its country,
// its currency — the payment listener's own setting — and the countries it
// sells to, checked against the processors), shipping (none by default; the
// countries shipped to and the shipping service), and what the payer sees.
// The store's settings are kept as the services' are, under _checkout
// (pay.go); the currency and store name are the payment listener's.

const (
	checkoutPath      = "/admin/checkout"
	checkoutPathTaken = "/admin/_checkout" // when the app has its own page at checkoutPath
)

// commerceListeners are the configurable listeners the page holds rather
// than Service Configuration: the payment and shipping listeners.
func (a *App) commerceListeners() (pay, ship []string) {
	for _, n := range a.configurable() {
		switch a.Reg.Routing.Listeners[n].Type {
		case "payment":
			pay = append(pay, n)
		case "shipping":
			ship = append(ship, n)
		}
	}
	return pay, ship
}

// checkoutOffered: the app takes payments, so the section is in the admin.
func (a *App) checkoutOffered() bool {
	pay, _ := a.commerceListeners()
	return len(pay) > 0
}

// storeOwned are the payment listener's settings the Store part holds: not
// shown on the processors' card, and not forgotten when that card is saved.
var storeOwned = map[string]bool{"currency": true, "brand_name": true}

type providerCheck struct {
	OK   bool
	Text string
}

type countryOption struct {
	Code, Name string
	Selected   bool
}

type currencyOption struct {
	Code, Name, Refused string
	Selected            bool
}

type storeView struct {
	Name, Currency, Country, SellsTo string
	NameFixed, CurrencyFixed         bool
	Countries, SellCountries         []countryOption
	Currencies                       []currencyOption
}

type shippingView struct {
	Ships     bool
	ShipTo    string
	Countries []countryOption
	Services  []serviceView
}

type methodView struct {
	Name, Kind, DefaultLabel, DefaultNote, Label, Note string
	Default                                            bool
}

type checkoutPage struct {
	Message, Error string
	NoKey, Live    bool
	Action         string
	Problems       []string
	Processors     []serviceView
	Checks         map[string]providerCheck
	Store          storeView
	Shipping       shippingView
	Methods        []methodView
	Button         string
}

// checkout serves the page and its saves.
func (a *App) checkout(rq *request) error {
	if ok, err := a.adminSection(rq, a.checkoutPath); !ok {
		return err
	}
	if !a.checkoutOffered() {
		return errNotFound
	}
	pg := &checkoutPage{NoKey: len(a.cfg.TokenKey) == 0, Action: a.url(a.checkoutPath)}
	if rq.r.Method == http.MethodPost {
		anchor, msg, err := a.saveCheckout(rq)
		var he *httpError
		switch {
		case errors.As(err, &he):
			return err
		case err != nil:
			pg.Error = err.Error()
		default:
			http.Redirect(rq.w, rq.r, a.url(a.checkoutPath)+"?done="+url.QueryEscape(msg)+"#"+anchor, http.StatusSeeOther)
			return nil
		}
	} else {
		a.syncSettings(rq.ctx())
		pg.Message = rq.r.URL.Query().Get("done")
	}
	a.fillCheckout(rq.ctx(), pg)
	a.site.Render(a.webRequest(rq), "checkout", "Payment Checkout", pg)
	return nil
}

// fillCheckout builds the page from the routing, what is stored, and the
// processors' answers.
func (a *App) fillCheckout(ctx context.Context, pg *checkoutPage) {
	payNames, shipNames := a.commerceListeners()
	views := map[string]serviceView{}
	for _, sv := range a.serviceViews(ctx) {
		views[sv.Name] = sv
	}
	cv := a.checkoutValues()
	pg.Checks = map[string]providerCheck{}
	var kinds []providerKind
	for _, n := range payNames {
		sv := views[n]
		sv.Fields, sv.Advanced = withoutStoreOwned(sv.Fields), withoutStoreOwned(sv.Advanced)
		for i := range sv.Providers {
			if c := payment.Countries(sv.Providers[i].Type); len(c) > 0 {
				sv.Providers[i].Countries = "Accounts in: " + strings.Join(c, ", ")
				if note := payment.CountriesNote(sv.Providers[i].Type); note != "" {
					sv.Providers[i].Countries += " — " + note
				}
			} else if note := payment.CountriesNote(sv.Providers[i].Type); note != "" {
				sv.Providers[i].Countries = note
			}
		}
		pg.Processors = append(pg.Processors, sv)
		merged := a.Reg.Settings(n)
		pg.Live = pg.Live || merged["mode"] == "live"
		for _, p := range listener.ProviderNames(merged["providers"]) {
			kind := merged[p+"_type"]
			if kind == "" {
				kind = p
			}
			kinds = append(kinds, providerKind{listener: n, name: p, kind: kind})
		}
	}
	accounts := a.verifyProviders(ctx, kinds)
	// The store
	first := ""
	if len(payNames) > 0 {
		first = payNames[0]
	}
	merged := a.Reg.Settings(first)
	routed := a.Reg.Routing.Listeners[first].Settings
	st := storeView{Name: merged["brand_name"], Currency: strings.ToUpper(merged["currency"]), Country: cv["country"], SellsTo: cv["sells_to"]}
	if st.Currency == "" {
		st.Currency = "USD"
	}
	_, st.NameFixed = routed["brand_name"]
	_, st.CurrencyFixed = routed["currency"]
	if st.SellsTo == "" {
		st.SellsTo = "all"
	}
	sell := set(shop.ParseCountries(cv["sell_countries"]))
	for _, c := range shop.Countries() {
		st.Countries = append(st.Countries, countryOption{Code: c.Code, Name: c.Name, Selected: c.Code == st.Country})
		st.SellCountries = append(st.SellCountries, countryOption{Code: c.Code, Name: c.Name, Selected: sell[c.Code]})
	}
	listed := false
	for _, c := range shop.Currencies() {
		o := currencyOption{Code: c.Code, Name: c.Name, Selected: c.Code == st.Currency, Refused: refusedBy(kinds, c.Code)}
		listed = listed || o.Selected
		st.Currencies = append(st.Currencies, o)
	}
	if !listed {
		st.Currencies = append([]currencyOption{{Code: st.Currency, Name: shop.CurrencyName(st.Currency), Selected: true, Refused: refusedBy(kinds, st.Currency)}}, st.Currencies...)
	}
	pg.Store = st
	// What the processors and the store's choices say of each other.
	for _, k := range kinds {
		key := k.name
		acc := accounts[k.listener+"/"+k.name]
		var bits []string
		ok := acc.err == nil
		switch {
		case acc.err != nil:
			bits = append(bits, "The keys don't work: "+acc.err.Error())
		case acc.skipped != "":
			bits = append(bits, acc.skipped)
		case acc.account == nil:
			bits = append(bits, "Can't be checked until a payment is made: its keys are only used at checkout.")
		default:
			s := "✓ Connected"
			if acc.account.Name != "" {
				s += " to " + acc.account.Name
			}
			if acc.account.Test {
				s += " (sandbox)"
			}
			if acc.account.Country != "" {
				s += " · " + shop.CountryName(acc.account.Country)
			}
			if len(acc.account.Currencies) > 0 {
				s += " · " + strings.Join(acc.account.Currencies, ", ")
			}
			bits = append(bits, s)
			if acc.account.Country != "" && st.Country != "" && acc.account.Country != st.Country {
				pg.Problems = append(pg.Problems, fmt.Sprintf("%s's account is in %s, but the store is in %s.", key, shop.CountryName(acc.account.Country), shop.CountryName(st.Country)))
			}
			if k.kind == "square" && len(acc.account.Currencies) > 0 && acc.account.Currencies[0] != st.Currency {
				ok = false
				pg.Problems = append(pg.Problems, fmt.Sprintf("%s's location takes %s only, but the store's currency is %s: payments through it would fail.", key, acc.account.Currencies[0], st.Currency))
			}
		}
		if !payment.TakesCurrency(k.kind, st.Currency) {
			ok = false
			pg.Problems = append(pg.Problems, fmt.Sprintf("%s doesn't take %s: turn it off, or choose another currency.", key, st.Currency))
		}
		if st.Country != "" && !payment.InCountry(k.kind, st.Country) {
			pg.Problems = append(pg.Problems, fmt.Sprintf("%s has no accounts in %s.", key, shop.CountryName(st.Country)))
		}
		pg.Checks[key] = providerCheck{OK: ok, Text: strings.Join(bits, " ")}
	}
	if st.Country == "" {
		pg.Problems = append(pg.Problems, "Choose the store's country (Store, below), so the processors can be checked against it.")
	}
	if st.SellsTo == "only" && len(sell) == 0 {
		pg.Problems = append(pg.Problems, "The store sells to no country: choose the countries it sells to, or All countries.")
	}
	// Shipping
	sh := shippingView{Ships: cv["ships"] == "1", ShipTo: cv["ship_to"]}
	if sh.ShipTo == "" {
		sh.ShipTo = "sells"
	}
	ship := set(shop.ParseCountries(cv["ship_countries"]))
	for _, c := range shop.Countries() {
		sh.Countries = append(sh.Countries, countryOption{Code: c.Code, Name: c.Name, Selected: ship[c.Code]})
	}
	for _, n := range shipNames {
		sh.Services = append(sh.Services, views[n])
	}
	pg.Shipping = sh
	// At checkout
	for _, k := range kinds {
		m := payment.MethodFor(k.name, k.kind)
		pg.Methods = append(pg.Methods, methodView{Name: k.name, Kind: k.kind, DefaultLabel: m.Label, DefaultNote: m.Note,
			Label: cv["label_"+k.name], Note: cv["note_"+k.name], Default: cv["default"] == k.name})
	}
	pg.Button = cv["button"]
}

func withoutStoreOwned(fs []ux.FormField) []ux.FormField {
	var out []ux.FormField
	for _, f := range fs {
		if !storeOwned[strings.TrimPrefix(f.Name, "s_")] {
			out = append(out, f)
		}
	}
	return out
}

func set(list []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range list {
		m[v] = true
	}
	return m
}

type providerKind struct{ listener, name, kind string }

// refusedBy names the processors in use that do not take a currency, "" when
// all do.
func refusedBy(kinds []providerKind, currency string) string {
	var out []string
	for _, k := range kinds {
		if !payment.TakesCurrency(k.kind, currency) {
			out = append(out, k.name)
		}
	}
	return strings.Join(out, ", ")
}

// verifyProvider asks a processor about its account (a test puts its own
// in its place: no test calls a processor).
var verifyProvider = payment.VerifyProvider

type accountCheck struct {
	account *payment.Account
	err     error
	skipped string
}

// verifyProviders asks each processor in use about its account, at once:
// the listener's own provider when it is open (live), else one opened from
// the stored keys for the asking (test mode).
func (a *App) verifyProviders(ctx context.Context, kinds []providerKind) map[string]accountCheck {
	out := map[string]accountCheck{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for _, k := range kinds {
		wg.Add(1)
		go func(k providerKind) {
			defer wg.Done()
			var c accountCheck
			merged := a.Reg.Settings(k.listener)
			own := payment.Settings{}
			for key, v := range merged {
				if strings.HasPrefix(key, k.name+"_") && key != k.name+"_type" {
					own[strings.TrimPrefix(key, k.name+"_")] = v
				}
			}
			if p, err := payment.OpenProvider(k.kind, k.name, own); err != nil {
				c.skipped = "Not set up yet: " + err.Error()
			} else {
				c.account, c.err = verifyProvider(ctx, p)
				if c.err != nil {
					c.err = errors.New(redact(c.err.Error(), own))
				}
			}
			mu.Lock()
			out[k.listener+"/"+k.name] = c
			mu.Unlock()
		}(k)
	}
	wg.Wait()
	return out
}

// saveCheckout carries out one of the page's forms: the anchor to come back
// to, what to say, or why not.
func (a *App) saveCheckout(rq *request) (anchor, msg string, err error) {
	f := rq.r.PostForm
	ctx := rq.ctx()
	payNames, shipNames := a.commerceListeners()
	switch f.Get("op") {
	case "service":
		name := f.Get("listener")
		var except map[string]bool
		ok := false
		for _, n := range payNames {
			if n == name {
				ok, except = true, storeOwned
			}
		}
		for _, n := range shipNames {
			ok = ok || n == name
		}
		if !ok {
			return "", "", errStatus(http.StatusBadRequest, "That service is not on this page")
		}
		if err := a.saveServiceExcept(rq, name, except); err != nil {
			return "", "", err
		}
		if err := a.Reg.Unconfigured(name); err != nil {
			return "svc-" + name, fmt.Sprintf("Saved %s. It is not ready yet: %v", name, err), nil
		}
		return "svc-" + name, "Saved " + name + ".", nil
	case "store":
		if len(payNames) == 0 {
			return "", "", errNotFound
		}
		country := strings.ToUpper(strings.TrimSpace(f.Get("country")))
		if country != "" && !shop.IsCountry(country) {
			return "", "", errors.New("choose the store's country from the list")
		}
		sellsTo := f.Get("sells_to")
		if sellsTo != "except" && sellsTo != "only" {
			sellsTo = "all"
		}
		sell := shop.ParseCountries(strings.Join(f["sell_country"], ","))
		if sellsTo == "only" && len(sell) == 0 {
			return "", "", errors.New("choose at least one country the store sells to, or All countries")
		}
		for _, n := range payNames {
			routed := a.Reg.Routing.Listeners[n].Settings
			merged := a.Reg.Settings(n)
			changed := map[string]*string{}
			if _, fixed := routed["currency"]; !fixed {
				cur := strings.ToUpper(strings.TrimSpace(f.Get("currency")))
				if len(cur) != 3 {
					return "", "", errors.New("choose the store's currency")
				}
				for _, p := range listener.ProviderNames(merged["providers"]) {
					kind := merged[p+"_type"]
					if kind == "" {
						kind = p
					}
					if !payment.TakesCurrency(kind, cur) {
						return "", "", fmt.Errorf("%s doesn't take %s. Turn %s off under Payment processors, or choose another currency", p, cur, p)
					}
				}
				if err := a.storeSetting(ctx, rq.p.ID, n, "currency", cur, false); err != nil {
					return "", "", err
				}
				changed["currency"] = &cur
			}
			if _, fixed := routed["brand_name"]; !fixed {
				name := strings.TrimSpace(f.Get("brand_name"))
				if name == "" {
					if err := a.forgetSetting(ctx, n, "brand_name"); err != nil {
						return "", "", err
					}
					changed["brand_name"] = nil
				} else {
					if err := a.storeSetting(ctx, rq.p.ID, n, "brand_name", name, false); err != nil {
						return "", "", err
					}
					changed["brand_name"] = &name
				}
			}
			a.reopenWith(ctx, n, changed, rq.p.ID)
		}
		if err := a.storeCheckout(ctx, rq.p.ID, map[string]string{"country": country, "sells_to": sellsTo, "sell_countries": strings.Join(sell, ",")}); err != nil {
			return "", "", err
		}
		return "store", "Saved the store.", nil
	case "shipping":
		ships := f.Get("no_shipping") != "1"
		shipTo := f.Get("ship_to")
		if shipTo != "only" {
			shipTo = "sells"
		}
		countries := shop.ParseCountries(strings.Join(f["ship_country"], ","))
		if ships && shipTo == "only" && len(countries) == 0 {
			return "", "", errors.New("choose at least one country the store ships to")
		}
		v := map[string]string{"ships": "", "ship_to": shipTo, "ship_countries": strings.Join(countries, ",")}
		if ships {
			v["ships"] = "1"
		}
		if err := a.storeCheckout(ctx, rq.p.ID, v); err != nil {
			return "", "", err
		}
		if ships {
			return "shipping", "Shipping is on.", nil
		}
		return "shipping", "No shipping: checkout asks for no shipping address.", nil
	case "methods":
		v := map[string]string{"default": strings.TrimSpace(f.Get("default")), "button": strings.TrimSpace(f.Get("button"))}
		for _, n := range payNames {
			for _, p := range listener.ProviderNames(a.Reg.Settings(n)["providers"]) {
				v["label_"+p] = strings.TrimSpace(f.Get("label_" + p))
				v["note_"+p] = strings.TrimSpace(f.Get("note_" + p))
			}
		}
		if err := a.storeCheckout(ctx, rq.p.ID, v); err != nil {
			return "", "", err
		}
		return "methods", "Saved the checkout's wording.", nil
	}
	return "", "", errStatus(http.StatusBadRequest, "Unknown form")
}

// storeCheckout keeps Payment Checkout settings ("" forgets one) and takes
// them at once on this node (the others read them within settingsEvery).
func (a *App) storeCheckout(ctx context.Context, by int64, v map[string]string) error {
	cur := a.checkoutValues()
	next := map[string]string{}
	for k, val := range cur {
		next[k] = val
	}
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		val := v[k]
		if val == "" {
			if err := a.forgetSetting(ctx, checkoutSettings, k); err != nil {
				return err
			}
			delete(next, k)
			continue
		}
		if err := a.storeSetting(ctx, by, checkoutSettings, k, val, false); err != nil {
			return err
		}
		next[k] = val
	}
	a.setCheckoutValues(next)
	return nil
}

// reopenWith opens a listener again with the stored settings and a save's
// own changes over them.
func (a *App) reopenWith(ctx context.Context, name string, changed map[string]*string, by int64) {
	values, _ := storedSettings(ctx, a.Reg, a.cfg.TokenKey, a.log)
	if err := a.Reg.Reopen(ctx, name, withChanges(values[name], changed), a.readyListener); err != nil {
		a.log.Warn("a service does not open with its settings", "listener", name, "by", by, "error", err)
	}
}

// redact takes a provider's own settings out of a processor's message (one
// that repeats the key it was given), so no key reaches the page.
func redact(msg string, own payment.Settings) string {
	for _, v := range own {
		if len(v) >= 6 {
			msg = strings.ReplaceAll(msg, v, "(the key you gave)")
		}
	}
	return msg
}
