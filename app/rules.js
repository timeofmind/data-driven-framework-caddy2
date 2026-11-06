// data-driven-framework-caddy2 — schema rules in the browser.
//
// The server builds one file per schema: this runtime followed by
// ddRules.start(<rules>), where <rules> is generator.ClientRules — each class's
// when / required_when / options / match / client checks, with expressions as
// trees (expr.Tree). Forms carry a marker <div data-dd-form="Class"
// data-dd-given="{…}"> (values the form has no input for, such as a parent's
// sport), field wrappers carry data-dd-field, and relation options carry
// data-dd-m (the target's matched values). The runtime hides fields that do
// not apply, switches required on and off, and narrows select lists as the
// form is filled in. It only narrows: every rule is enforced again on save,
// and the form works without it.
//
// The evaluator follows the expr package exactly (null arithmetic, loose
// equality, Go rounding); expr/conformance.json is run against both.
var ddRules = (function () {
  "use strict";

  function Unknown(name) { this.message = "unknown name " + name; }

  function truthy(v) {
    if (v === null || v === undefined) return false;
    if (typeof v === "boolean") return v;
    if (typeof v === "number") return v !== 0;
    if (typeof v === "string") return v !== "" && v !== "0";
    return true;
  }

  var numRE = /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/;
  function toNum(v) {
    if (typeof v === "number") return [v, true];
    if (typeof v === "boolean") return [v ? 1 : 0, true];
    if (typeof v === "string" && numRE.test(v)) return [Number(v), true];
    return [0, false];
  }

  function compare(l, r) {
    if (l === null || r === null) return [0, false];
    var a = toNum(l), b = toNum(r);
    if (a[1] && b[1]) return [a[0] < b[0] ? -1 : a[0] > b[0] ? 1 : 0, true];
    if (typeof l === "string" && typeof r === "string") return [l < r ? -1 : l > r ? 1 : 0, true];
    return [0, false];
  }

  function equal(l, r) {
    if (l === null || r === null) return l === null && r === null;
    if (typeof l === "boolean") return l === truthy(r);
    if (typeof r === "boolean") return r === truthy(l);
    var c = compare(l, r);
    return c[1] && c[0] === 0;
  }

  // Go's math.Round: halves away from zero.
  function goRound(x) {
    var t = Math.trunc(x);
    return Math.abs(x - t) >= 0.5 ? t + Math.sign(x) : t;
  }

  function format(v) {
    if (v === null) return "";
    if (typeof v === "boolean") return v ? "true" : "false";
    if (typeof v === "number") {
      var s = String(v);
      if (s.indexOf("e") < 0) return s;
      return v.toLocaleString("en-US", { useGrouping: false, maximumFractionDigits: 20 });
    }
    return String(v);
  }

  function pad(n) { return n < 10 ? "0" + n : String(n); }

  // Dates are YYYY-MM-DD strings (a datetime's time is ignored), counted in
  // whole UTC days so daylight-saving changes cannot shift them.
  var dateRE = /^(\d{4})-(\d{2})-(\d{2})/;
  function toDate(v) {
    var m = typeof v === "string" && dateRE.exec(v);
    if (!m) return null;
    var t = Date.UTC(+m[1], +m[2] - 1, +m[3]), d = new Date(t);
    if (d.getUTCFullYear() !== +m[1] || d.getUTCMonth() !== +m[2] - 1 || d.getUTCDate() !== +m[3]) return null;
    return Math.round(t / 86400000);
  }
  function fromDays(n) {
    var d = new Date(n * 86400000);
    return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate());
  }
  function formatDuration(sec) {
    var neg = "";
    if (sec < 0) { neg = "-"; sec = -sec; }
    var h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
    return h > 0 ? neg + h + ":" + pad(m) + ":" + pad(s) : neg + m + ":" + pad(s);
  }

  function norm(v) { return v === undefined ? null : v; }

  // evaluate runs a tree with env(name) → {ok, value}; it throws Unknown for
  // names env does not know, as Go returns an error.
  function evaluate(n, env) {
    switch (n[0]) {
      case "lit": return norm(n[1]);
      case "id": {
        var got = env(n[1]);
        if (!got.ok) throw new Unknown(n[1]);
        return norm(got.value);
      }
      case "un": {
        var x = evaluate(n[2], env);
        if (n[1] === "not") return !truthy(x);
        return typeof x === "number" ? -x : null;
      }
      case "bin": return binary(n[1], n[2], n[3], env);
      case "call": return call(n[1], n.slice(2), env);
    }
    throw new Error("bad node " + n[0]);
  }

  function binary(op, ln, rn, env) {
    var l = evaluate(ln, env), r;
    if (op === "and") return truthy(l) ? truthy(evaluate(rn, env)) : false;
    if (op === "or") return truthy(l) ? true : truthy(evaluate(rn, env));
    r = evaluate(rn, env);
    switch (op) {
      case "==": return equal(l, r);
      case "!=": return !equal(l, r);
      case "<": case "<=": case ">": case ">=": {
        var c = compare(l, r);
        if (!c[1]) return false;
        return op === "<" ? c[0] < 0 : op === "<=" ? c[0] <= 0 : op === ">" ? c[0] > 0 : c[0] >= 0;
      }
    }
    if (op === "+") {
      if (l === null || r === null) return null;
      if (typeof l === "string" || typeof r === "string") return format(l) + format(r);
    }
    if (typeof l !== "number" || typeof r !== "number") return null;
    switch (op) {
      case "+": return l + r;
      case "-": return l - r;
      case "*": return l * r;
      case "/": return r === 0 ? null : l / r;
    }
    throw new Error("bad operator " + op);
  }

  function call(fn, argNodes, env) {
    if (fn === "if") return truthy(evaluate(argNodes[0], env)) ? evaluate(argNodes[1], env) : evaluate(argNodes[2], env);
    var a = argNodes.map(function (x) { return evaluate(x, env); }), i;
    switch (fn) {
      case "coalesce":
        for (i = 0; i < a.length; i++) {
          if (a[i] !== null && a[i] !== "" && a[i] !== 0) return a[i];
        }
        return null;
      case "round": {
        if (typeof a[0] !== "number") return null;
        var p = Math.pow(10, a.length === 2 && typeof a[1] === "number" ? a[1] : 0);
        return goRound(a[0] * p) / p;
      }
      case "duration": return typeof a[0] === "number" ? formatDuration(goRound(a[0])) : null;
      case "lower": return typeof a[0] === "string" ? a[0].toLowerCase() : null;
      case "upper": return typeof a[0] === "string" ? a[0].toUpperCase() : null;
      case "abs": return typeof a[0] === "number" ? Math.abs(a[0]) : null;
      case "add_days": {
        var d = toDate(a[0]);
        if (d === null || typeof a[1] !== "number") return null;
        return fromDays(d + goRound(a[1]));
      }
      case "days_between": {
        var x = toDate(a[0]), y = toDate(a[1]);
        return x === null || y === null ? null : y - x;
      }
      case "min": case "max": {
        var best = null;
        for (i = 0; i < a.length; i++) {
          if (a[i] === null) continue;
          if (best === null) { best = a[i]; continue; }
          var c = compare(a[i], best);
          if (c[1] && ((fn === "min" && c[0] < 0) || (fn === "max" && c[0] > 0))) best = a[i];
        }
        return best;
      }
    }
    throw new Error("unknown function " + fn);
  }

  // holds mirrors Go's "err == nil && Truthy(v)".
  function holds(tree, env) {
    try { return truthy(evaluate(tree, env)); } catch (e) { return false; }
  }

  // parseDuration mirrors runtime.ParseDuration for h:mm:ss, m:ss and seconds.
  function parseDuration(s) {
    s = s.trim();
    if (s === "") return null;
    if (numRE.test(s) && Number(s) >= 0) return goRound(Number(s));
    if (s.indexOf(":") >= 0) {
      var parts = s.split(":"), total = 0;
      if (parts.length > 3) return null;
      for (var i = 0; i < parts.length; i++) {
        if (!numRE.test(parts[i])) return null;
        var f = Number(parts[i]);
        if (f < 0 || (i > 0 && f >= 60)) return null;
        total = total * 60 + f;
      }
      return goRound(total);
    }
    var m = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/.exec(s.replace(/ /g, ""));
    if (m && (m[1] || m[2] || m[3])) return (+m[1] || 0) * 3600 + (+m[2] || 0) * 60 + (+m[3] || 0);
    return null;
  }

  function inputValue(el, type) {
    if (el.disabled && el.hasAttribute("data-dd-off")) return null; // cleared on save
    if (type === "boolean") return el.type === "checkbox" ? el.checked : truthy(el.value);
    var s = el.value;
    if (s.trim() === "") return null;
    if (type === "integer" || type === "float") {
      var n = Number(s);
      return isFinite(n) ? n : null;
    }
    if (type === "duration") return parseDuration(s);
    return s;
  }

  function wrapperOf(form, name) {
    return form.querySelector('[data-dd-field="' + name + '"]');
  }

  function setRequired(form, name, el, on) {
    if (!el.hasAttribute("data-dd-req")) el.setAttribute("data-dd-req", el.required ? "1" : "0");
    on = on || el.getAttribute("data-dd-req") === "1";
    el.required = on;
    var w = wrapperOf(form, name), label = w && w.querySelector("label"), mark = label && label.querySelector(".req");
    if (on && label && !mark) {
      mark = document.createElement("span");
      mark.className = "req";
      mark.setAttribute("aria-hidden", "true");
      mark.setAttribute("data-dd", "1");
      mark.textContent = "*";
      label.appendChild(document.createTextNode(" "));
      label.appendChild(mark);
    } else if (!on && mark && mark.getAttribute("data-dd") === "1") {
      if (mark.previousSibling && mark.previousSibling.nodeType === 3) label.removeChild(mark.previousSibling);
      label.removeChild(mark);
    }
  }

  // narrow shows only the options keep() accepts; a chosen option that no
  // longer fits is replaced by the empty choice (or the first that fits).
  function narrow(sel, keep) {
    var changed = false, first = null, blank = null, i, o;
    for (i = 0; i < sel.options.length; i++) {
      o = sel.options[i];
      if (o.value === "") { blank = blank || o; continue; }
      var ok = keep(o);
      o.hidden = !ok;
      o.disabled = !ok;
      if (ok && !first) first = o;
    }
    o = sel.options[sel.selectedIndex];
    if (o && o.disabled) {
      var to = blank || first;
      if (to) { to.selected = true; changed = true; }
    }
    return changed;
  }

  function bind(form, marker, cls) {
    var given = {};
    try { given = JSON.parse(marker.getAttribute("data-dd-given") || "{}"); } catch (e) {}
    function env(name) {
      var el = form.elements.namedItem(name);
      if (el && el.nodeType === 1 && el.name === name) return { ok: true, value: inputValue(el, cls.types[name]) };
      if (Object.prototype.hasOwnProperty.call(given, name)) return { ok: true, value: norm(given[name]) };
      return { ok: false };
    }
    function apply() {
      var changed = false, name, rule, el;
      for (name in cls.fields || {}) {
        rule = cls.fields[name];
        el = form.elements.namedItem(name);
        if (!el || el.nodeType !== 1) continue;
        var applies = !rule.when || holds(rule.when, env);
        var w = wrapperOf(form, name);
        if (w) {
          w.hidden = !applies;
          w.style.display = applies ? "" : "none"; // themes may style .field over [hidden]
        }
        if (applies) {
          if (el.hasAttribute("data-dd-off")) { el.disabled = false; el.removeAttribute("data-dd-off"); }
        } else if (!el.disabled) {
          el.disabled = true;
          el.setAttribute("data-dd-off", "1");
        }
        setRequired(form, name, el, applies && !!rule.required_when && holds(rule.required_when, env));
        if (rule.options && el.tagName === "SELECT") {
          var allowed = null;
          for (var i = 0; i < rule.options.length; i++) {
            if (holds(rule.options[i].when, env)) { allowed = rule.options[i].values; break; }
          }
          changed = narrow(el, function (o) { return !allowed || allowed.indexOf(o.value) >= 0; }) || changed;
        }
      }
      for (name in cls.match || {}) {
        el = form.elements.namedItem(name);
        if (!el || el.tagName !== "SELECT") continue;
        var conds = [];
        cls.match[name].forEach(function (m) {
          try {
            var v = evaluate(m.expr, env);
            if (v !== null) conds.push([m.slot, v]);
          } catch (e) {}
        });
        changed = narrow(el, function (o) {
          var d = {};
          try { d = JSON.parse(o.getAttribute("data-dd-m") || "{}"); } catch (e) {}
          return conds.every(function (c) { return equal(norm(d[c[0]]), c[1]); });
        }) || changed;
      }
      // A group (fieldset) whose fields are all hidden is hidden too.
      var sets = form.querySelectorAll("fieldset");
      for (var f = 0; f < sets.length; f++) {
        var ws = sets[f].querySelectorAll("[data-dd-field]"), any = ws.length === 0;
        for (var k = 0; k < ws.length && !any; k++) any = !ws[k].hidden;
        sets[f].style.display = any ? "" : "none";
      }
      (cls.checks || []).forEach(function (ck) {
        el = ck.slot && form.elements.namedItem(ck.slot);
        if (el && el.setCustomValidity) el.setCustomValidity(holds(ck.expr, env) ? "" : ck.message);
      });
      return changed;
    }
    function run() {
      for (var i = 0; i < 4 && apply(); i++) {} // a narrowed list can change what else applies
    }
    form.addEventListener("input", run);
    form.addEventListener("change", run);
    run();
  }

  function start(rules) {
    if (typeof document === "undefined") return;
    var go = function () {
      var marks = document.querySelectorAll("[data-dd-form]");
      for (var i = 0; i < marks.length; i++) {
        var cls = rules[marks[i].getAttribute("data-dd-form")], form = marks[i].closest("form");
        if (cls && form) bind(form, marks[i], cls);
      }
    };
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", go);
    else go();
  }

  return { evaluate: evaluate, truthy: truthy, equal: equal, parseDuration: parseDuration, start: start };
})();
