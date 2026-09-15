// Query page. Results are inserted with textContent only, never as HTML.
"use strict";
(function () {
  var form = document.getElementById("form");
  var query = document.getElementById("query");
  var format = document.getElementById("format");
  var token = document.getElementById("token");
  var status = document.getElementById("status");
  var results = document.getElementById("results");
  var busy = false;

  function el(tag, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    return e;
  }

  function setStatus(text, isError) {
    status.textContent = text;
    status.className = isError ? "error" : "";
  }

  function cell(term) {
    var td = el("td");
    if (!term) return td;
    if (term.type === "uri") {
      td.className = "uri";
      td.textContent = "<" + term.value + ">";
    } else if (term.type === "bnode") {
      td.textContent = "_:" + term.value;
    } else if (term.type === "triple") {
      td.textContent = "<< " + JSON.stringify(term.value) + " >>";
    } else {
      td.textContent = term.value;
      if (term["xml:lang"]) td.appendChild(el("span", "@" + term["xml:lang"], "dt"));
      else if (term.datatype) td.appendChild(el("span", " ^^" + term.datatype, "dt"));
    }
    return td;
  }

  function renderJSON(data) {
    if (typeof data.boolean === "boolean") {
      results.appendChild(el("pre", String(data.boolean)));
      return 1;
    }
    var vars = (data.head && data.head.vars) || [];
    var rows = (data.results && data.results.bindings) || [];
    var table = el("table");
    var head = el("tr");
    vars.forEach(function (v) { head.appendChild(el("th", "?" + v)); });
    table.appendChild(el("thead")).appendChild(head);
    var body = el("tbody");
    rows.forEach(function (b) {
      var tr = el("tr");
      vars.forEach(function (v) { tr.appendChild(cell(b[v])); });
      body.appendChild(tr);
    });
    table.appendChild(body);
    results.appendChild(table);
    return rows.length;
  }

  function run() {
    if (busy) return;
    busy = true;
    results.textContent = "";
    setStatus("Running…");
    var headers = { "Content-Type": "application/sparql-query", "Accept": format.value };
    if (token.value) headers["Authorization"] = "Bearer " + token.value;
    var started = performance.now();
    fetch("sparql", { method: "POST", headers: headers, body: query.value, credentials: "omit" })
      .then(function (resp) {
        var ct = resp.headers.get("Content-Type") || "";
        return resp.text().then(function (text) { return { resp: resp, ct: ct, text: text }; });
      })
      .then(function (r) {
        var ms = Math.round(performance.now() - started);
        if (!r.resp.ok) {
          setStatus(r.resp.status + " " + r.resp.statusText + " (" + ms + " ms)", true);
          results.appendChild(el("pre", r.text));
          return;
        }
        if (r.ct.indexOf("application/sparql-results+json") === 0) {
          var n = renderJSON(JSON.parse(r.text));
          setStatus(n + " result" + (n === 1 ? "" : "s") + " in " + ms + " ms");
        } else {
          results.appendChild(el("pre", r.text));
          setStatus(r.ct + ", " + r.text.length + " characters in " + ms + " ms");
        }
      })
      .catch(function (err) { setStatus(String(err), true); })
      .then(function () { busy = false; });
  }

  form.addEventListener("submit", function (e) { e.preventDefault(); run(); });
  query.addEventListener("keydown", function (e) {
    if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); run(); }
  });
})();
