package app

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// adminToken is the _admin token a sign-up address carries, as the page's
// form posts it back.
func adminToken(t *testing.T, loc string) string {
	t.Helper()
	u, err := url.Parse(loc)
	if err != nil || u.Query().Get("_admin") == "" {
		t.Fatalf("no admin token in %q", loc)
	}
	return u.Query().Get("_admin")
}

var inviteRE = regexp.MustCompile(`value="[^"]*(/_dd/admin\?token=[^"]+)"`)

func adminID(t *testing.T, a *App, login string) string {
	t.Helper()
	r, err := a.adminRecords().First(context.Background(), runtime.Query{Where: []runtime.Cond{runtime.Eq("login", login)}})
	if err != nil {
		t.Fatalf("no admin record for %s: %v", login, err)
	}
	return strconv.FormatInt(r.ID(), 10)
}

// The Admins page: the configuration's logins are the owners (level 0);
// admins are added by login with a level, kept in the app's data, and reach
// the sections whose level is above their own. An admin changes only admins
// of less authority than theirs.
func TestAdminsPage(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	os.WriteFile(filepath.Join(dir, "admin.yaml"), []byte("sections:\n  - {label: Walks, path: /c/walks, level: 3}\n"), 0o644)
	a, srv := newServicesApp(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	owner, bea, cat, dan := newBrowser(t, srv), newBrowser(t, srv), newBrowser(t, srv), newBrowser(t, srv)
	owner.signup("admin@example.com", "Owner")
	bea.signup("bea@example.com", "Bea")
	cat.signup("cat@example.com", "Cat")
	dan.signup("dan@example.com", "Dan")

	// A member is not an admin: no admin area, and a section with a level is not theirs.
	for _, p := range []string{"/admin/admins", "/admin/services", "/admin/media", "/c/walks"} {
		if code, _, _ := bea.get(p); code != http.StatusForbidden {
			t.Fatalf("a member at %s: %d", p, code)
		}
	}
	code, body, _ := owner.get("/admin/admins")
	if code != http.StatusOK || !strings.Contains(body, "admin@example.com") || !strings.Contains(body, "level 0") || !strings.Contains(body, ">Admins</a>") {
		t.Fatalf("the owner's page: %d\n%s", code, body)
	}
	if !strings.Contains(body, "owners and level 1") || !strings.Contains(body, "owners and levels 1 to 2") || !strings.Contains(body, "every admin") {
		t.Fatalf("what each level reaches:\n%s", body)
	}

	add := func(b *browser, login, level string) (int, string, string) {
		return b.post("/admin/admins", url.Values{"action": {"add"}, "login": {login}, "level": {level}})
	}
	// The owner makes Bea a level-1 admin: an account that exists is one at once.
	if code, body, loc := add(owner, " Bea@Example.com ", "1"); code != http.StatusSeeOther || !strings.Contains(loc, "done=") {
		t.Fatalf("adding bea: %d %s\n%s", code, loc, extractAlert(body))
	}
	for _, p := range []string{"/admin/admins", "/admin/services", "/admin/media", "/c/walks"} {
		if code, _, _ := bea.get(p); code != http.StatusOK {
			t.Fatalf("a level-1 admin at %s: %d", p, code)
		}
	}
	// Twice, an owner, a level out of range, a level not below one's own: refused, and said.
	for what, try := range map[string][3]string{
		"twice":         {"owner", "bea@example.com", "2"},
		"an owner":      {"owner", "admin@example.com", "2"},
		"level 0":       {"owner", "cat@example.com", "0"},
		"level 100":     {"owner", "cat@example.com", "100"},
		"a peer by bea": {"bea", "cat@example.com", "1"},
	} {
		b := owner
		if try[0] == "bea" {
			b = bea
		}
		if code, body, _ := add(b, try[1], try[2]); code != http.StatusOK || !strings.Contains(body, `class="alert"`) {
			t.Fatalf("%s: %d\n%s", what, code, body)
		}
	}
	// Bea gives less than her own: Cat at level 3.
	if code, body, _ := add(bea, "cat@example.com", "3"); code != http.StatusSeeOther {
		t.Fatalf("bea adding cat: %d\n%s", code, extractAlert(body))
	}
	// Level 3 reaches Media (any admin's) — not Services or Admins (level 2) or Walks (level 3):
	// a page of the server's sends her to a section she has, an app's page refuses.
	if code, _, _ := cat.get("/admin/media"); code != http.StatusOK {
		t.Fatalf("cat at media: %d", code)
	}
	for _, p := range []string{"/admin/services", "/admin/admins"} {
		if code, _, h := cat.get(p); code != http.StatusSeeOther || h.Get("Location") != "/admin/media" {
			t.Fatalf("cat at %s: %d %s", p, code, h.Get("Location"))
		}
	}
	if code, _, _ := cat.get("/c/walks"); code != http.StatusForbidden {
		t.Fatalf("cat at a level-3 section: %d", code)
	}
	if _, body, _ := cat.get("/admin/media"); strings.Contains(body, ">Services</a>") || strings.Contains(body, ">Admins</a>") || !strings.Contains(body, ">Media</a>") {
		t.Fatalf("cat's admin navigation:\n%s", body)
	}
	if code, _, _ := cat.post("/admin/admins", url.Values{"_csrf": {cat.csrf("/admin/media")}, "action": {"remove"}, "id": {adminID(t, a, "bea@example.com")}}); code != http.StatusForbidden {
		t.Fatalf("cat changing the admins: %d", code)
	}
	// Bea cannot touch herself (a peer) — nor give Cat her own level; she can move Cat to 2.
	beaID, catID := adminID(t, a, "bea@example.com"), adminID(t, a, "cat@example.com")
	if code, _, _ := bea.post("/admin/admins", url.Values{"action": {"remove"}, "id": {beaID}}); code != http.StatusForbidden {
		t.Fatalf("bea removing herself: %d", code)
	}
	if code, body, _ := bea.post("/admin/admins", url.Values{"action": {"level"}, "id": {catID}, "level": {"1"}}); code != http.StatusOK || !strings.Contains(body, `class="alert"`) {
		t.Fatalf("bea giving her own level: %d", code)
	}
	if code, body, _ := bea.post("/admin/admins", url.Values{"action": {"level"}, "id": {catID}, "level": {"2"}}); code != http.StatusSeeOther {
		t.Fatalf("bea setting cat to 2: %d\n%s", code, extractAlert(body))
	}
	if code, _, _ := cat.get("/c/walks"); code != http.StatusOK {
		t.Fatalf("cat, now level 2, at a level-3 section: %d", code)
	}
	// Removed, Cat is a member again; the changes are on the page.
	if code, _, _ := owner.post("/admin/admins", url.Values{"action": {"remove"}, "id": {catID}}); code != http.StatusSeeOther {
		t.Fatalf("removing cat: %d", code)
	}
	if code, _, _ := cat.get("/admin/media"); code != http.StatusForbidden {
		t.Fatalf("cat, removed, at media: %d", code)
	}
	_, body, _ = owner.get("/admin/admins")
	for _, want := range []string{"made bea@example.com an admin, level 1", "set cat@example.com to level 2", "removed cat@example.com (level 2)"} {
		if !strings.Contains(body, want) {
			t.Fatalf("the changes lack %q:\n%s", want, body)
		}
	}
	_ = dan
}

// Someone with no account is invited: the page gives a link, they are an
// admin once they sign up through it, and an account made with the login
// any other way is not made one.
func TestAdminInvitation(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, false) // sign-up is off: the link is the way in
	os.RemoveAll(filepath.Join(dir, "theme"))
	a, srv := newServicesApp(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	owner := newBrowser(t, srv)
	if code, _, h := owner.get(strings.TrimPrefix(AdminLink(a.cfg.TokenKey, "admin@example.com", srv.URL, 0), srv.URL)); code != http.StatusSeeOther {
		t.Fatalf("the owner's link: %d", code)
	} else if code, body, _ := owner.post(h.Get("Location"), url.Values{"_admin": {adminToken(t, h.Get("Location"))}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {"Owner"}}); code != http.StatusSeeOther {
		t.Fatalf("the owner's sign-up: %d\n%s", code, extractAlert(body))
	}
	code, body, _ := owner.post("/admin/admins", url.Values{"action": {"add"}, "login": {"eve@example.com"}, "level": {"2"}})
	m := inviteRE.FindStringSubmatch(body)
	if code != http.StatusOK || m == nil || !strings.Contains(body, "invited") {
		t.Fatalf("inviting eve: %d\n%s", code, body)
	}
	link := html.UnescapeString(m[1])
	if _, body, _ := owner.get("/admin/admins"); !strings.Contains(body, "invited — no account yet") || strings.Contains(body, "/_dd/admin?token=") {
		t.Fatalf("the list after an invitation (the link is shown once):\n%s", body)
	}
	// The link leads to sign-up with the login fixed, whatever the form says.
	eve := newBrowser(t, srv)
	code, _, h := eve.get(link)
	if code != http.StatusSeeOther || !strings.Contains(h.Get("Location"), "/signup?") {
		t.Fatalf("eve's link: %d %s", code, h.Get("Location"))
	}
	if code, body, _ := eve.post(h.Get("Location"), url.Values{"_admin": {adminToken(t, h.Get("Location"))}, "email": {"mallory@example.com"}, "pw": {"long enough pw"}, "password_confirm": {"long enough pw"}, "name": {"Eve"}}); code != http.StatusSeeOther {
		t.Fatalf("eve's sign-up: %d\n%s", code, extractAlert(body))
	}
	// Level 2: Media, not Services; and the invitation is used up.
	if code, _, _ := eve.get("/admin/media"); code != http.StatusOK {
		t.Fatalf("eve at media: %d", code)
	}
	if code, _, _ := eve.get("/admin/services"); code != http.StatusSeeOther {
		t.Fatalf("eve at services: %d", code)
	}
	if code, _, _ := newBrowser(t, srv).get(link); code != http.StatusForbidden {
		t.Fatalf("the link, used again: %d", code)
	}
	if _, body, _ := owner.get("/admin/admins"); !strings.Contains(body, "eve@example.com took up the invitation, level 2") || strings.Contains(body, "invited — no account yet") {
		t.Fatalf("after eve joined:\n%s", body)
	}
	// Withdrawn before it is used, a link is dead.
	_, body, _ = owner.post("/admin/admins", url.Values{"action": {"add"}, "login": {"fay@example.com"}, "level": {"5"}})
	fay := html.UnescapeString(inviteRE.FindStringSubmatch(body)[1])
	if code, _, _ := owner.post("/admin/admins", url.Values{"action": {"remove"}, "id": {adminID(t, a, "fay@example.com")}}); code != http.StatusSeeOther {
		t.Fatalf("withdrawing fay: %d", code)
	}
	if code, _, _ := newBrowser(t, srv).get(fay); code != http.StatusForbidden {
		t.Fatalf("a withdrawn invitation's link: %d", code)
	}
}

// An account made with an invited login by ordinary sign-up is not an
// admin, and the invitation's link then signs nobody in.
func TestAdminInvitationNotBySignup(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	_, srv := newServicesApp(t, dir, []byte("0123456789abcdef0123456789abcdef"))
	owner := newBrowser(t, srv)
	owner.signup("admin@example.com", "Owner")
	_, body, _ := owner.post("/admin/admins", url.Values{"action": {"add"}, "login": {"gil@example.com"}, "level": {"1"}})
	link := html.UnescapeString(inviteRE.FindStringSubmatch(body)[1])
	someone := newBrowser(t, srv)
	someone.signup("gil@example.com", "Not Gil")
	if code, _, _ := someone.get("/admin/media"); code != http.StatusForbidden {
		t.Fatalf("an account made by plain sign-up with an invited login: %d", code)
	}
	if code, _, _ := someone.get(link); code != http.StatusForbidden {
		t.Fatalf("the link once such an account exists: %d", code)
	}
	if code, _, _ := someone.get("/admin/media"); code != http.StatusForbidden {
		t.Fatalf("after the link: %d", code)
	}
}
