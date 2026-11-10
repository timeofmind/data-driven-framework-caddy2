package app

import (
	"fmt"
	"html"
	"net/http"
	"sort"
	"strings"

	"github.com/timeofmind/data-driven-framework-go/uml"
)

// mermaidScript is the renderer the diagram pages load (pinned).
const mermaidScript = "https://cdn.jsdelivr.net/npm/mermaid@11.4.1/dist/mermaid.min.js"

// serveUML serves the application's UML diagrams under the configured path:
// an index of what there is, each diagram as a page rendered by mermaid.js,
// and its Mermaid text (.mmd) for other tools.
func (a *App) serveUML(rq *request, rest string) {
	if a.cfg.UMLPath == "" {
		http.NotFound(rq.w, rq.r)
		return
	}
	if a.cfg.UMLAdmins && (rq.p == nil || !rq.p.Admin) {
		a.renderError(rq, errForbidden)
		return
	}
	rest = strings.Trim(rest, "/")
	w := rq.w
	if rest == "" {
		a.umlIndex(rq)
		return
	}
	key, raw := rest, false
	if strings.HasSuffix(key, ".mmd") {
		key, raw = strings.TrimSuffix(key, ".mmd"), true
	}
	title, text, ok := uml.Render(key, a.Model, a.Pages, a.Reg.Routing)
	if !ok {
		http.NotFound(w, rq.r)
		return
	}
	text = a.withoutFrameworkClasses(text)
	if raw {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(text))
		return
	}
	// The renderer is a script from a CDN: this page alone allows it.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self' https://cdn.jsdelivr.net; frame-ancestors 'none'; base-uri 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s — %s</title>
<style>body{margin:0;font:14px system-ui,sans-serif;color:#1a1a1a;background:#fff}header{padding:12px 20px;border-bottom:1px solid #e3e3e3;display:flex;gap:16px;align-items:baseline}header a{color:#0a58ca;text-decoration:none}main{padding:20px;overflow:auto}pre.mermaid{margin:0}.muted{color:#666}@media (prefers-color-scheme:dark){body{color:#eee;background:#111}header{border-color:#333}header a{color:#8ab4f8}.muted{color:#aaa}}</style>
</head><body><header><a href="%s">All diagrams</a><strong>%s</strong><span class="muted">%s</span><a href="%s.mmd">Mermaid text</a></header>
<main><pre class="mermaid">%s</pre></main>
<script src="%s"></script>
<script>mermaid.initialize({startOnLoad:true,securityLevel:'strict',theme:window.matchMedia('(prefers-color-scheme: dark)').matches?'dark':'default'});</script>
</body></html>`, html.EscapeString(title), html.EscapeString(a.man.Name), a.url(a.cfg.UMLPath), html.EscapeString(title), html.EscapeString(a.man.Name),
		a.url(a.cfg.UMLPath+"/"+key), html.EscapeString(text), mermaidScript)
}

func (a *App) umlIndex(rq *request) {
	w := rq.w
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	ds := uml.Catalogue(a.Model, a.Pages, a.Reg.Routing)
	sort.SliceStable(ds, func(i, j int) bool {
		ci, cj := strings.HasPrefix(ds[i].Key, "class:"), strings.HasPrefix(ds[j].Key, "class:")
		if ci != cj {
			return !ci
		}
		return ds[i].Title < ds[j].Title
	})
	var b strings.Builder
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>UML — %s</title>
<style>body{margin:0;font:15px system-ui,sans-serif;color:#1a1a1a;background:#fff}main{max-width:48rem;margin:0 auto;padding:24px 20px}h1{font-size:1.4rem}h2{font-size:1rem;margin-top:1.6em;color:#666}ul{list-style:none;padding:0}li{padding:8px 0;border-bottom:1px solid #eee}a{color:#0a58ca;text-decoration:none}a:hover{text-decoration:underline}.d{color:#666;font-size:.9em}@media (prefers-color-scheme:dark){body{color:#eee;background:#111}li{border-color:#333}a{color:#8ab4f8}h2,.d{color:#aaa}}</style>
</head><body><main><h1>%s — UML</h1><p class="d">Diagrams generated from the application's data model, pages and routing. Each page renders in the browser; the Mermaid text is a link away.</p>`,
		html.EscapeString(a.man.Name), html.EscapeString(a.man.Name))
	section := ""
	for _, d := range ds {
		if frameworkClass(strings.TrimPrefix(d.Key, "class:")) {
			continue
		}
		s := "Overview"
		if strings.HasPrefix(d.Key, "class:") {
			s = "One class and its neighbours"
		}
		if s != section {
			section = s
			fmt.Fprintf(&b, "<h2>%s</h2><ul>", s)
		}
		fmt.Fprintf(&b, `<li><a href="%s">%s</a> <span class="d">%s</span></li>`, a.url(a.cfg.UMLPath+"/"+d.Key), html.EscapeString(d.Title), html.EscapeString(d.Description))
	}
	b.WriteString("</ul></main></body></html>")
	w.Write([]byte(b.String()))
}

// withoutFrameworkClasses takes the framework's own classes (sessions, feeds)
// out of a diagram: they are the server's, not the application's.
func (a *App) withoutFrameworkClasses(text string) string {
	var out []string
	skip := false
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if skip {
			if t == "}" {
				skip = false
			}
			continue
		}
		if strings.HasPrefix(t, "class Framework") || strings.HasPrefix(t, "Framework") && strings.Contains(t, "{") {
			if strings.HasSuffix(t, "{") {
				skip = true
			}
			continue
		}
		if strings.Contains(t, "Framework") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}
