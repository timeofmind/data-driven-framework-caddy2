package app

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/timeofmind/data-driven-framework-go/generator"
	"github.com/timeofmind/data-driven-framework-go/runtime"
)

// The schema's rules in the browser (see rules.js): one script per schema,
// built when the app loads and served at /_dd/rules.<hash>.js with a
// year-long immutable cache — a schema change gives it a new name.

//go:embed rules.js
var rulesRuntime []byte

type clientRules struct {
	classes map[string]*generator.ClientClass
	script  []byte
	name    string // rules.<hash>.js
}

func buildClientRules(a *App) error {
	a.rules = &clientRules{classes: generator.ClientRules(a.Model)}
	if len(a.rules.classes) == 0 {
		return nil
	}
	data, err := json.Marshal(a.rules.classes)
	if err != nil {
		return err
	}
	js := append(append([]byte{}, rulesRuntime...), "ddRules.start("...)
	js = append(append(js, data...), ");\n"...)
	sum := sha256.Sum256(js)
	a.rules.script, a.rules.name = js, "rules."+hex.EncodeToString(sum[:6])+".js"
	return nil
}

func (a *App) serveRules(rq *request, name string) {
	if a.rules.script == nil || name != a.rules.name {
		http.NotFound(rq.w, rq.r)
		return
	}
	h := rq.w.Header()
	h.Set("Content-Type", "text/javascript; charset=utf-8")
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	rq.w.Write(a.rules.script)
}

// formRules is what a form needs to load the rules: the script, the class,
// and the values its rules read that the form has no input for.
type formRules struct {
	Script, Class, Given string
}

func (a *App) formRules(ctx context.Context, rec *runtime.Record) *formRules {
	cc := a.rules.classes[rec.Class.Name]
	if cc == nil {
		return nil
	}
	given, err := json.Marshal(a.Reg.GivenValues(ctx, rec, cc.Given))
	if err != nil {
		return nil
	}
	return &formRules{Script: a.url("/_dd/" + a.rules.name), Class: rec.Class.Name, Given: string(given)}
}
