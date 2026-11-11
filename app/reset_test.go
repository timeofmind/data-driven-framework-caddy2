package app

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/timeofmind/data-driven-framework-go/listener/mail"
)

// The password reset: /forgot saves a reset record for an account, which
// the routing sends to the mail listener — the template from the database
// when there is one of that name, else the app's file — with a link;
// /reset?code=… names the account, takes the new password once, and signs
// the person in. The answer to /forgot never says whether an account exists.
func TestPasswordReset(t *testing.T) {
	// The fixture with a routing that sends resets to a mail listener (the
	// test captures what it would send).
	dir := t.TempDir()
	if err := copyDir(fixtureApp, dir); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, "routing"), 0o755)
	os.WriteFile(filepath.Join(dir, "routing", "walkclub.yaml"), []byte(`
listeners:
  db:   {type: sqlite, dsn: `+filepath.Join(dir, "club.db")+`}
  mail: {type: smtp, host: 127.0.0.1, port: 1, starttls: off, from: club@example.org, template_class: MailTemplate, templates: mail}
routes:
  default: {source: db, flows_to: [db]}
  PasswordReset: {source: db, flows_to: [db, {listener: mail, template: password-reset}]}
`), 0o644)
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	a, srv := newTestAppAt(t, dir)
	var ml *mail.Listener
	for _, l := range a.Reg.Listeners {
		if m, ok := l.(*mail.Listener); ok {
			ml = m
		}
	}
	if ml == nil {
		t.Fatal("no mail listener")
	}
	type sent struct{ to, subject, body string }
	mails := make(chan sent, 4)
	ml.Send = func(ctx context.Context, to, subject, body string) error {
		mails <- sent{to, subject, body}
		return nil
	}
	ann := newBrowser(t, srv)
	ann.signup("ann@example.com", "Ann")
	ann.post("/logout", url.Values{"_csrf": {ann.csrf("/c/routes/new")}})
	if code, body, _ := ann.get("/login"); code != 200 || !strings.Contains(body, "/forgot") {
		t.Errorf("the sign-in page offers the forgotten-password link: %d", code)
	}
	// An unknown login: the same answer, no mail.
	code, body, _ := ann.post("/forgot", url.Values{"login": {"nobody@example.com"}})
	if code != 200 || !strings.Contains(body, "is on its way") {
		t.Errorf("forgot, unknown: %d %s", code, extractAlert(body))
	}
	select {
	case m := <-mails:
		t.Errorf("a mail for an unknown login: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
	// The account: a mail with the link, from the app's file template.
	code, body, _ = ann.post("/forgot", url.Values{"login": {"ann@example.com"}})
	if code != 200 || !strings.Contains(body, "is on its way") {
		t.Errorf("forgot: %d %s", code, extractAlert(body))
	}
	var m sent
	select {
	case m = <-mails:
	case <-time.After(3 * time.Second):
		t.Fatal("no mail")
	}
	linkRE := regexp.MustCompile(`/reset\?code=([0-9a-f]{32})`)
	codes := linkRE.FindStringSubmatch(m.body)
	if m.to != "ann@example.com" || !strings.Contains(m.subject, "Reset your Walk Club password") || codes == nil || !strings.Contains(m.body, "Hello Ann") {
		t.Fatalf("the mail: %+v", m)
	}
	// The reset page names the account; a mismatch is refused; the new password is set and signs in.
	code, body, _ = ann.get("/reset?code=" + codes[1])
	if code != 200 || !strings.Contains(body, "ann@example.com") || !strings.Contains(body, `name="password"`) {
		t.Errorf("the reset page: %d", code)
	}
	tok := ann.csrf("/reset?code=" + codes[1]) // the token comes from the page with the form, not a bare /reset
	code, body, _ = ann.post("/reset", url.Values{"_csrf": {tok}, "code": {codes[1]}, "password": {"a new password"}, "password_confirm": {"another"}})
	if code != 200 || !strings.Contains(body, "passwords don") {
		t.Errorf("mismatch: %d", code)
	}
	code, _, loc := ann.post("/reset", url.Values{"_csrf": {tok}, "code": {codes[1]}, "password": {"a new password"}, "password_confirm": {"a new password"}})
	if code != 303 || loc == "" {
		t.Fatalf("reset: %d %s", code, loc)
	}
	if code, body, _ := ann.get("/me"); code != 303 && !strings.Contains(body, "Ann") {
		t.Errorf("signed in after the reset: %d", code)
	}
	if code, body, _ := ann.get("/reset?code=" + codes[1]); code != 200 || !strings.Contains(body, "not valid any more") {
		t.Errorf("the code spent: %d", code)
	}
	ben := newBrowser(t, srv)
	if code, body, _ := ben.post("/login", url.Values{"login": {"ann@example.com"}, "password": {"long enough pw"}}); code != 200 || !strings.Contains(body, "match an account") {
		t.Errorf("the old password: %d", code)
	}
	if code, _, loc := ben.post("/login", url.Values{"login": {"ann@example.com"}, "password": {"a new password"}}); code != 303 || loc == "" {
		t.Errorf("the new password: %d %s", code, loc)
	}
	// A template in the database replaces the file's.
	tm := a.Reg.MustClass("MailTemplate")
	if err := tm.Save(context.Background(), tm.New(map[string]any{"name": "password-reset", "to": "{{.member.email}}", "subject": "From the database", "body": "{{.base_url}}/reset?code={{.record.code}} {{fill \"for {who}\"}}", "vars": "who: \"{{.member.name}}\""})); err != nil {
		t.Fatal(err)
	}
	ben.post("/forgot", url.Values{"login": {"ann@example.com"}})
	select {
	case m = <-mails:
	case <-time.After(3 * time.Second):
		t.Fatal("no mail from the database template")
	}
	if m.subject != "From the database" || !strings.Contains(m.body, "for Ann") || !linkRE.MatchString(m.body) {
		t.Errorf("the database template: %+v", m)
	}
}

func copyDir(from, to string) error {
	return filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, p)
		dst := filepath.Join(to, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(dst, b, 0o644)
	})
}
