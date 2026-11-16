package app

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A database that refuses what was saved: the change is kept in the change
// log, and every page of the admin area tells the admins it is not stored
// — until the database takes it.
func TestAdminNoticeWhenTheDatabaseRefuses(t *testing.T) {
	if os.Getenv("DDF_TEST_MYSQL_DSN") != "" {
		t.Skip("the routed fixture is SQLite")
	}
	noticeAfter = 0
	defer func() { noticeAfter = time.Minute }()
	dir := servicesFixture(t, true)
	os.RemoveAll(filepath.Join(dir, "theme"))
	a, err := New(context.Background(), Config{AppDir: dir, Driver: "sqlite", DSN: filepath.Join(dir, "club.db"), Admins: []string{"admin@example.com"},
		TokenKey: []byte("0123456789abcdef0123456789abcdef"), LogDir: filepath.Join(dir, "log"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a)
	t.Cleanup(func() { srv.Close(); a.Close() })
	owner := newBrowser(t, srv)
	owner.signup("admin@example.com", "Owner")
	said := func() bool {
		_, body, _ := owner.get("/admin/admins")
		return strings.Contains(body, "Saved changes are not reaching")
	}
	wait := func(what string, want bool) {
		t.Helper()
		for i := 0; i < 400; i++ {
			if said() == want {
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}
	wait("a quiet admin area", false)
	ctx := context.Background()
	table := a.auth.Class.EffectiveTable()
	if _, err := a.Reg.DB.ExecContext(ctx, `CREATE TRIGGER refuse BEFORE INSERT ON "`+table+`" BEGIN SELECT RAISE(ABORT, 'the table will not take it'); END`); err != nil {
		t.Fatal(err)
	}
	newBrowser(t, srv).signup("bea@example.com", "Bea") // saved: kept in the log, refused by the database
	wait("the admin area to say the database refuses", true)
	if _, body, _ := owner.get("/admin/services"); !strings.Contains(body, "the table will not take it") || !strings.Contains(body, "Nothing is lost") {
		t.Fatalf("what the notice says:\n%s", body)
	}
	if _, body, _ := owner.get("/"); strings.Contains(body, "Saved changes are not reaching") {
		t.Fatal("the notice is shown outside the admin area")
	}
	if _, err := a.Reg.DB.ExecContext(ctx, `DROP TRIGGER refuse`); err != nil {
		t.Fatal(err)
	}
	wait("the notice to go once the database takes it", false)
}
