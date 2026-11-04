// Command caddy is the standard Caddy server with the datadriven handler
// compiled in. Build it with `go build ./cmd/caddy` — no xcaddy needed.
package main

import (
	caddycmd "github.com/caddyserver/caddy/v2/cmd"

	_ "github.com/caddyserver/caddy/v2/modules/standard"

	_ "github.com/timeofmind/data-driven-framework-caddy2"
)

func main() {
	caddycmd.Main()
}
