// Shared helpers of the TenantScaler dashboard: DOM building (text is
// always inserted with textContent, never parsed as HTML), number
// formatting and JSON requests.
"use strict";

window.TS = window.TS || {};

TS.util = (function () {
  // el builds an element: attrs sets attributes (class, data-*, aria-*,
  // hidden...), children are nodes or strings (inserted as text).
  function el(tag, attrs, children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs || {})) {
      if (v === false || v === null || v === undefined) continue;
      node.setAttribute(k, v === true ? "" : String(v));
    }
    for (const c of [].concat(children === undefined ? [] : children)) {
      if (c === null || c === undefined) continue;
      node.appendChild(typeof c === "string" || typeof c === "number" ? document.createTextNode(String(c)) : c);
    }
    return node;
  }

  // setText replaces an element's content with text.
  function setText(node, text) {
    node.textContent = text === null || text === undefined ? "" : String(text);
  }

  // clear removes every child of node.
  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  function isNum(v) {
    return typeof v === "number" && Number.isFinite(v);
  }

  // num formats v with the given decimals; "n/a" when it is not a number.
  function num(v, digits) {
    return isNum(v) ? v.toFixed(digits === undefined ? 2 : digits) : "n/a";
  }

  // sec formats seconds: "850 ms" below one second, "1.24 s" above.
  function sec(v) {
    if (!isNum(v)) return "n/a";
    return v < 1 ? Math.round(v * 1000) + " ms" : v.toFixed(2) + " s";
  }

  // pct formats a percentage already in 0-100.
  function pct(v, digits) {
    return isNum(v) ? v.toFixed(digits === undefined ? 1 : digits) + "%" : "n/a";
  }

  // cssVar reads a custom property as resolved on node (theme aware).
  function cssVar(node, name) {
    return getComputedStyle(node).getPropertyValue(name).trim();
  }

  // requestJSON sends a request and resolves to {status, body}; network
  // failures and timeouts reject.
  async function requestJSON(method, url, body, timeoutMs) {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), timeoutMs || 10000);
    try {
      const init = { method: method, signal: ctl.signal, headers: {} };
      if (body !== undefined) {
        init.headers["Content-Type"] = "application/json";
        init.body = JSON.stringify(body);
      }
      const resp = await fetch(url, init);
      let data = null;
      const text = await resp.text();
      if (text) {
        try {
          data = JSON.parse(text);
        } catch (e) {
          data = { error: "invalid JSON from " + url };
        }
      }
      return { status: resp.status, body: data };
    } finally {
      clearTimeout(timer);
    }
  }

  // getJSON GETs url and resolves to its body; any non-200 rejects with
  // the server's error message.
  async function getJSON(url, timeoutMs) {
    const r = await requestJSON("GET", url, undefined, timeoutMs);
    if (r.status !== 200) {
      throw new Error((r.body && r.body.error) || "HTTP " + r.status + " from " + url);
    }
    return r.body;
  }

  return { el, setText, clear, isNum, num, sec, pct, cssVar, requestJSON, getJSON };
})();
