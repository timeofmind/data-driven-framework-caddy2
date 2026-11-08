// Calendar rescheduling: drag an entry you may edit (data-dd-move, its
// reschedule URL) onto another day (data-date) and it moves there. The server
// decides — it checks permission and the schema's rules — and the entry moves
// on the page only once the server has saved it.
(function () {
  "use strict";
  var cal = document.querySelector("[data-dd-calendar]");
  if (!cal) return;
  var csrf = cal.getAttribute("data-dd-csrf") || "";
  var status = document.createElement("p");
  status.className = "cal-status";
  status.setAttribute("role", "status");
  status.hidden = true;
  cal.parentNode.insertBefore(status, cal);
  var dragging = null;

  function say(text, bad) {
    status.textContent = text;
    status.hidden = !text;
    status.classList.toggle("bad", !!bad);
  }
  function dayOf(el) {
    return el && el.closest ? el.closest("[data-date]") : null;
  }

  cal.addEventListener("dragstart", function (e) {
    var it = e.target.closest && e.target.closest("[data-dd-move]");
    if (!it) return;
    dragging = it;
    it.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", it.getAttribute("href") || "");
  });
  cal.addEventListener("dragend", function () {
    if (dragging) dragging.classList.remove("dragging");
    dragging = null;
    var over = cal.querySelector(".drop");
    if (over) over.classList.remove("drop");
  });
  cal.addEventListener("dragover", function (e) {
    var d = dayOf(e.target);
    if (!dragging || !d) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = "move";
    var over = cal.querySelector(".drop");
    if (over !== d) {
      if (over) over.classList.remove("drop");
      d.classList.add("drop");
    }
  });
  cal.addEventListener("drop", function (e) {
    var d = dayOf(e.target);
    var it = dragging;
    if (!it || !d) return;
    e.preventDefault();
    d.classList.remove("drop");
    var date = d.getAttribute("data-date");
    if (dayOf(it) === d) return;
    var body = new URLSearchParams();
    body.set("_csrf", csrf);
    body.set("date", date);
    it.classList.add("saving");
    fetch(it.getAttribute("data-dd-move"), {
      method: "POST",
      body: body,
      credentials: "same-origin",
      headers: { "Accept": "application/json" }
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        it.classList.remove("saving");
        if (!r.ok) {
          say("Couldn't move “" + it.textContent + "”: " + (j.error || "the server refused (" + r.status + ")."), true);
          return;
        }
        // An entry covering several days shows on each; redraw for those.
        if (it.getAttribute("data-dd-days") !== "1") {
          location.reload();
          return;
        }
        d.appendChild(it);
        say("Moved “" + it.textContent + "” to " + date + ".");
      });
    }, function () {
      it.classList.remove("saving");
      say("Couldn't move “" + it.textContent + "”: no connection.", true);
    });
  });
})();
