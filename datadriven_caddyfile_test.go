package datadriven

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

// The database directive gives the DSN itself; database_file names a file
// holding it.
func TestCaddyfileDatabase(t *testing.T) {
	for src, want := range map[string]Handler{
		"datadriven /chess {\n app /a\n database sqlite /var/lib/chess/chess.db\n}": {BasePath: "/chess", App: "/a", Driver: "sqlite", DSN: "/var/lib/chess/chess.db"},
		"datadriven {\n app /a\n database_file mysql /etc/app/database.dsn\n}":      {App: "/a", Driver: "mysql", DSNFile: "/etc/app/database.dsn"},
	} {
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(src)); err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if h.BasePath != want.BasePath || h.App != want.App || h.Driver != want.Driver || h.DSN != want.DSN || h.DSNFile != want.DSNFile {
			t.Errorf("%s:\n got %+v\nwant %+v", src, h, want)
		}
	}
}
