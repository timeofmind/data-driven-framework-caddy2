package app

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dop251/goja"
	"github.com/timeofmind/data-driven-framework-go/expr"
)

// TestRulesJSConformance runs the framework's expression conformance cases
// through rules.js, so the browser evaluator cannot drift from Go's.
func TestRulesJSConformance(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(string(rulesRuntime)); err != nil {
		t.Fatal(err)
	}
	run, ok := goja.AssertFunction(vm.Get("ddRules").ToObject(vm).Get("evaluate"))
	if !ok {
		t.Fatal("ddRules.evaluate missing")
	}
	var cases []struct {
		Expr string         `json:"expr"`
		Env  map[string]any `json:"env"`
		Want any            `json:"want"`
	}
	if err := json.Unmarshal(expr.Conformance, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		e, err := expr.Parse(c.Expr)
		if err != nil {
			t.Fatal(err)
		}
		tree, _ := json.Marshal(e.Tree())
		var treeVal any
		json.Unmarshal(tree, &treeVal)
		env := func(call goja.FunctionCall) goja.Value {
			v, ok := c.Env[call.Argument(0).String()]
			return vm.ToValue(map[string]any{"ok": ok, "value": v})
		}
		got, err := run(goja.Undefined(), vm.ToValue(treeVal), vm.ToValue(env))
		if err != nil {
			t.Errorf("%s: %v", c.Expr, err)
			continue
		}
		g := got.Export()
		switch n := g.(type) {
		case int64:
			g = float64(n)
		}
		if !reflect.DeepEqual(g, c.Want) {
			t.Errorf("JS %s with %v = %#v, want %#v", c.Expr, c.Env, g, c.Want)
		}
	}
}

func TestRulesJSParseDuration(t *testing.T) {
	vm := goja.New()
	if _, err := vm.RunString(string(rulesRuntime)); err != nil {
		t.Fatal(err)
	}
	for in, want := range map[string]any{"90": int64(90), "1:30": int64(90), "1:02:05": int64(3725), "1h2m5s": int64(3725), "2m": int64(120), "1:75": nil, "": nil, "x": nil} {
		v, err := vm.RunString(`ddRules.parseDuration(` + strconvQuote(in) + `)`)
		if err != nil {
			t.Fatal(err)
		}
		if got := v.Export(); !reflect.DeepEqual(got, want) {
			t.Errorf("parseDuration(%q) = %#v, want %#v", in, got, want)
		}
	}
}

func strconvQuote(s string) string { b, _ := json.Marshal(s); return string(b) }
