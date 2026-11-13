package datadriven

import (
	"fmt"
	"os"
	"strings"

	"github.com/caddyserver/caddy/v2"
	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"

	"github.com/timeofmind/data-driven-framework-caddy2/app"
)

// caddy admin-link mints a short-lived link that signs an admin in to an
// app's admin backend (its Service Configuration page), for the builder
// that made the app and for an operator.
func init() {
	caddycmd.RegisterCommand(caddycmd.Command{
		Name:  "admin-link",
		Usage: "--token-key <file> --login <email> --base-url <url> [--ttl 10m]",
		Short: "Mints a short-lived link that signs an admin in to an app's admin backend",
		Long: `Mints a link to <base-url>/_dd/admin signed with the app's token key (the
file the datadriven handler's token_key_file names), naming one of the app's
admin logins. Opening the link signs that account in; when it has no account
yet it leads to sign-up with the login fixed, allowed even when sign-up is
off. The link is good for --ttl (10 minutes by default) on any node sharing
the key, and is not single-use. The base URL is the app's public address,
base path included.`,
		CobraFunc: func(cmd *cobra.Command) {
			cmd.Flags().String("token-key", "", "the token key file (token_key_file)")
			cmd.Flags().String("login", "", "the admin login to sign in (one of the handler's admins)")
			cmd.Flags().String("base-url", "", "the app's public URL")
			cmd.Flags().Duration("ttl", app.AdminLinkTTL, "how long the link is good for")
			cmd.RunE = caddycmd.WrapCommandFuncForCobra(cmdAdminLink)
		},
	})
}

func cmdAdminLink(fl caddycmd.Flags) (int, error) {
	keyFile, login, base := fl.String("token-key"), fl.String("login"), fl.String("base-url")
	ttl := fl.Duration("ttl")
	if keyFile == "" || login == "" || base == "" {
		return caddy.ExitCodeFailedStartup, fmt.Errorf("--token-key, --login and --base-url are required")
	}
	b, err := os.ReadFile(keyFile)
	if err != nil {
		return caddy.ExitCodeFailedStartup, fmt.Errorf("token key: %w", err)
	}
	key := []byte(strings.TrimSpace(string(b)))
	if len(key) == 0 {
		return caddy.ExitCodeFailedStartup, fmt.Errorf("token key: %s is empty", keyFile)
	}
	if ttl <= 0 {
		ttl = app.AdminLinkTTL
	}
	fmt.Println(app.AdminLink(key, login, base, ttl))
	return caddy.ExitCodeSuccess, nil
}
