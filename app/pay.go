package app

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/listener"
	"github.com/timeofmind/data-driven-framework-go/listener/payment"
	"github.com/timeofmind/data-driven-framework-go/model"
	"github.com/timeofmind/data-driven-framework-go/runtime"
	"github.com/timeofmind/data-driven-framework-go/ux"
)

// The payment step's server side (the framework's docs/commerce.md, "The
// payment step"): what a form creating a payment offers (the methods the
// store turned on, in its order, in its words), the payment page
// (/_dd/pay/<resource>/<id>) that takes the payer on to the processor, and
// the way back (/_dd/pay/<resource>/<id>/return and /cancel), where the
// payment is marked approved with the processor's token so the listener
// captures it — the same in every app, with no script of the app's own.

const payPath = "/_dd/pay"

// paymentListenerFor is the payment listener a class's records flow to, nil
// when they flow to none.
func (a *App) paymentListenerFor(class string) (string, *payment.Listener) {
	rt := a.Reg.Routing
	if rt == nil {
		return "", nil
	}
	r, ok := rt.Routes[class]
	if !ok {
		return "", nil
	}
	for _, d := range r.FlowsTo {
		if d.Listener == "" {
			continue
		}
		if pl, ok := listener.Unwrap(a.Reg.Listeners[d.Listener]).(*payment.Listener); ok {
			return d.Listener, pl
		}
	}
	return "", nil
}

// payOffer is what a form creating a record of class offers the payer: nil
// unless the class flows to a payment listener that is open.
func (a *App) payOffer(class string) *ux.PayOffer {
	name, pl := a.paymentListenerFor(class)
	if pl == nil {
		return nil
	}
	_ = name
	words := a.checkoutValues()
	o := &ux.PayOffer{Field: "provider", Default: pl.Default(), Test: pl.Mode() == "test"}
	if d := words["default"]; d != "" {
		o.Default = d
	}
	for _, m := range pl.Methods() {
		label, note := m.Label, m.Note
		if v := words["label_"+m.Name]; v != "" && m.Name != "" {
			label = v
		}
		if v := words["note_"+m.Name]; v != "" && m.Name != "" {
			note = v
		}
		pm := ux.PayMethod{Value: m.Name, Kind: m.Kind, Label: label, Note: note, Flow: m.Flow}
		switch m.Flow {
		case "redirect":
			pm.Submit = "Continue to " + label
		case "button":
			pm.Submit = "Continue to " + label
		case "test":
			pm.Submit = "Pay (test — nothing charged)"
			if m.Kind != "test" {
				pm.Note = note + " Test mode: nothing will be charged."
			}
		}
		if v := words["button"]; v != "" {
			pm.Submit = v
		}
		o.Methods = append(o.Methods, pm)
	}
	// The offline ways to pay the store turned on (an e-Transfer, in person …).
	for _, m := range payment.OfflineMethods() {
		if words[m.Name] != "1" {
			continue
		}
		label, note := m.Label, m.Note
		if v := words["label_"+m.Name]; v != "" {
			label = v
		}
		if v := words["note_"+m.Name]; v != "" {
			note = v
		}
		o.Methods = append(o.Methods, ux.PayMethod{Value: m.Name, Kind: m.Kind, Label: label, Note: note, Flow: m.Flow, Submit: offlineSubmit[m.Name]})
	}
	if len(o.Methods) > 1 && o.Methods[0].Kind == "test" {
		o.Methods = o.Methods[1:] // offline methods offered: no stand-in test method beside them
		o.Test = false
	}
	return o
}

// payReturnVia is the return address the payment listeners give the
// processors: this app's /_dd/pay/<resource>/<id>/<step>.
func (a *App) payReturnVia(class string, id int64, step string) string {
	res := strings.ToLower(class)
	if c := a.Model.Class(class); c != nil {
		res = generator.ResourcePath(c)
	}
	return fmt.Sprintf("%s%s/%s/%d/%s", strings.TrimRight(a.cfg.BaseURL, "/"), a.url(payPath), res, id, step)
}

// paymentClassByResource is the class of a payment page's resource: one the
// routing sends to a payment listener.
func (a *App) paymentClassByResource(res string) *model.Class {
	for _, c := range a.Model.Classes() {
		if generator.ResourcePath(c) == res {
			if _, pl := a.paymentListenerFor(c.Name); pl != nil {
				return c
			}
		}
	}
	return nil
}

// payHookPath is where processors' webhooks reach the app:
// /_dd/pay/hook/<listener>/<provider> (payment.Listener.WebhookURL).
const payHookPath = payPath + "/hook/"

// payHook takes a processor's webhook. Its body is not trusted: the
// listener only asks the processor about the payments it names (Notify).
// Nothing is signed in and no CSRF token is asked for; the answer is 200
// for anything addressed to a listener and provider in use, so the
// processor doesn't retry, and 404 otherwise.
func (a *App) payHook(w http.ResponseWriter, r *http.Request, rest string) {
	name, provider, ok := strings.Cut(strings.Trim(rest, "/"), "/")
	if !ok || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	pl, _ := listener.Unwrap(a.Reg.Listeners[name]).(*payment.Listener)
	if pl == nil || pl.WebhookURL(provider) == "" {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "unreadable", http.StatusBadRequest)
		return
	}
	var form url.Values
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		form, _ = url.ParseQuery(string(body))
	}
	n := pl.Notify(r.Context(), provider, body, form)
	a.log.Info("payment webhook", "listener", name, "provider", provider, "payments", n)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}

// payPage is what the payment page shows.
type payPage struct {
	API, Then, Step, Say, Detail, Back, ApproveURL string
	ShowLinks                                      bool
}

// pay serves /_dd/pay/<resource>/<id>[/return|/cancel].
func (a *App) pay(rq *request, rest string) error {
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		return errNotFound
	}
	c := a.paymentClassByResource(parts[0])
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if c == nil || err != nil || id <= 0 {
		return errNotFound
	}
	step := "start"
	if len(parts) == 3 {
		if parts[2] != "return" && parts[2] != "cancel" {
			return errNotFound
		}
		step = parts[2]
	}
	q := rq.r.URL.Query()
	if step == "return" {
		// Back from the processor: the listener asks it now how the payment
		// stands, whoever is signed in here (or no one), so the payment is
		// settled even when the approval below can't be recorded.
		if _, pl := a.paymentListenerFor(c.Name); pl != nil {
			pl.Nudge(c.Name, id, 5*time.Second)
		}
	}
	if rq.p == nil {
		// Signed out on the way back (a session that ended at the processor):
		// signing in brings them back here.
		if a.auth == nil {
			return errNotFound
		}
		http.Redirect(rq.w, rq.r, a.url("/login")+"?next="+url.QueryEscape(rq.r.URL.RequestURI()), http.StatusSeeOther)
		return nil
	}
	h, err := a.Reg.Class(c.Name)
	if err != nil {
		return err
	}
	rec, err := h.Load(rq.ctx(), id)
	if err != nil {
		return errNotFound
	}
	if ok, _ := a.Reg.Readable(rq.ctx(), rec, rq.p); !ok {
		return errNotFound
	}
	then := a.sameSite(q.Get("then"))
	if then == "" {
		then = a.sameSite(q.Get("to"))
	}
	if then == "" {
		then = a.url("/")
	}
	if step == "return" || step == "cancel" {
		if step == "return" {
			if err := a.approvePayment(rq, rec, q); err != nil {
				a.log.Warn("payment return", "class", c.Name, "id", id, "error", err)
			}
		}
		u := fmt.Sprintf("%s/%s/%d?step=%s&then=%s", a.url(payPath), parts[0], id, step, url.QueryEscape(then))
		http.Redirect(rq.w, rq.r, u, http.StatusSeeOther)
		return nil
	}
	if s := q.Get("step"); s == "return" || s == "cancel" {
		step = s
	}
	pg := &payPage{API: a.url("/api/v1/" + parts[0] + "/" + strconv.FormatInt(id, 10)), Then: then, Step: step, Back: "Back"}
	switch step {
	case "return":
		pg.Say, pg.Detail = "Finishing your payment…", "Please keep this page open for a moment."
	case "cancel":
		pg.Say, pg.Detail, pg.Back, pg.ShowLinks = "Payment cancelled", "You left the payment page before paying. Nothing was charged.", "Back to try again", true
	default:
		pg.Say, pg.Detail = "Starting your payment…", "One moment while we connect to the payment service."
		if u, _ := rec.Get("approve_url").(string); u != "" {
			pg.ApproveURL = u
		}
	}
	a.site.Render(a.webRequest(rq), "pay", "Payment", pg)
	return nil
}

// sameSite is u when it is a path of this app (or a full URL of its own
// address), else "": where the payer is sent on is never another site.
func (a *App) sameSite(u string) string {
	if u == "" {
		return ""
	}
	if base := strings.TrimRight(a.cfg.BaseURL, "/"); base != "" && strings.HasPrefix(u, base+"/") {
		u = strings.TrimPrefix(u, base)
	}
	if !strings.HasPrefix(u, "/") || strings.HasPrefix(u, "//") || strings.ContainsAny(u, "\\\r\n") {
		return ""
	}
	if p := a.cfg.BasePath; p != "" && !strings.HasPrefix(u, p+"/") && u != p {
		u = p + u
	}
	return u
}

// approvalTokenParams are the references processors put on the return URL:
// PayPal's token, Stripe's session, Square's order, Mollie's and Amazon
// Pay's checkout session.
var approvalTokenParams = []string{"amazonCheckoutSessionId", "token", "session_id", "payment_intent", "orderId", "transactionId"}

// approvePayment marks a payment waiting at the processor approved, with
// the processor's reference: the listener then captures it. A payment past
// that (already approved, captured, failed) is left as it is.
//
// The payment's status is the processor's and the admins' (the payer may
// not write it): the server records the approval for the payer — theirs
// alone (the payment's owner), and never for an offline method, which the
// store records received itself.
func (a *App) approvePayment(rq *request, rec *runtime.Record, q url.Values) error {
	if st, _ := rec.Get("status").(string); st != "approval_pending" {
		return nil
	}
	if prov, _ := rec.Get("provider").(string); payment.IsOffline(prov) {
		return nil
	}
	if owner := ownerOf(rec.Class); owner != "" && rec.Int(owner) != rq.p.ID && !rq.p.Admin {
		return fmt.Errorf("not the payer's payment")
	}
	rec.Values["status"] = "approved"
	for _, k := range approvalTokenParams {
		if v := q.Get(k); v != "" {
			if rec.Class.Slot("approval_token") != nil {
				rec.Values["approval_token"] = v
			}
			break
		}
	}
	h, err := a.Reg.Class(rec.Class.Name)
	if err != nil {
		return err
	}
	return h.Save(rq.ctx(), rec)
}

// checkoutSettings is the name the Payment Checkout page's own settings are
// kept under (FrameworkServiceSetting.listener): the store's country, the
// countries it sells and ships to, the methods' words. No listener has it.
const checkoutSettings = "_checkout"

// checkoutValues are the Payment Checkout settings as last read.
func (a *App) checkoutValues() map[string]string {
	a.checkoutMu.Lock()
	defer a.checkoutMu.Unlock()
	return a.checkoutVals
}

func (a *App) setCheckoutValues(v map[string]string) {
	out := map[string]string{}
	for k, val := range v {
		out[k] = val
	}
	a.checkoutMu.Lock()
	a.checkoutVals = out
	a.checkoutMu.Unlock()
}

// shopValues are the store's settings pages read as shop.*: whether it
// ships, its currency (the payment listener's), its country.
func (a *App) shopValues() map[string]any {
	cv := a.checkoutValues()
	out := map[string]any{"ships": cv["ships"] == "1"}
	for _, k := range []string{"etransfer_to", "etransfer_note", "in_person_note", "bank_details", "cheque_note"} {
		if v := cv[k]; v != "" {
			out[k] = v
		}
	}
	if c := cv["country"]; c != "" {
		out["country"] = c
	}
	for _, l := range a.Reg.Listeners {
		if pl, ok := listener.Unwrap(l).(*payment.Listener); ok {
			out["currency"] = pl.Currency()
			break
		}
	}
	return out
}

// offlineSubmit is the payment step's button for an offline way to pay.
var offlineSubmit = map[string]string{
	"offline_etransfer": "Place order — pay by e-Transfer",
	"offline_in_person": "Place order — pay in person",
	"offline_bank":      "Place order — pay by bank transfer",
	"offline_cheque":    "Place order — pay by cheque",
}
