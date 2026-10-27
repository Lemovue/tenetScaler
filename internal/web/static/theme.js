// Theme of the dashboard ("dark" or "light"), applied in <head> without
// defer so the page never paints in the other theme first. First visit: the
// system's prefers-color-scheme decides; a choice made with the spider
// toggle is stored in this browser and wins from then on. Every change
// fires "ts:theme" on document so canvases (charts, webs) redraw.
"use strict";

window.TS = window.TS || {};

TS.theme = (function () {
  const KEY = "tenantscaler-theme";
  const root = document.documentElement;
  const mq = window.matchMedia("(prefers-color-scheme: dark)");

  function stored() {
    try {
      const v = localStorage.getItem(KEY);
      return v === "dark" || v === "light" ? v : null;
    } catch (e) {
      return null; // storage blocked: follow the system
    }
  }

  function system() {
    return mq.matches ? "dark" : "light";
  }

  function apply(theme) {
    root.dataset.theme = theme;
    document.dispatchEvent(new CustomEvent("ts:theme"));
  }

  // choose applies theme and stores it as the viewer's choice.
  function choose(theme) {
    try {
      localStorage.setItem(KEY, theme);
    } catch (e) {
      // storage blocked: the choice lasts until the page reloads
    }
    apply(theme);
  }

  root.dataset.theme = stored() || system();
  if (mq.addEventListener) mq.addEventListener("change", () => stored() || apply(system()));

  return { current: () => root.dataset.theme, choose };
})();
