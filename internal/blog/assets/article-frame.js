(function () {
  "use strict";

  if (document.currentScript.hasAttribute("data-oss-frame-child")) {
    const parentOrigin = new URL(document.currentScript.src).origin;
    let pending = false;

    function reportSize() {
      if (pending) return;
      pending = true;
      requestAnimationFrame(() => {
        pending = false;
        const body = document.body;
        const height = Math.max(body.getBoundingClientRect().height, body.scrollHeight);
        parent.postMessage({ type: "oss-article-height", height: Math.ceil(height) + 1 }, parentOrigin);
      });
    }

    window.addEventListener("message", (event) => {
      if (event.source !== parent || event.origin !== parentOrigin) return;
      const data = event.data;
      if (data?.type !== "oss-article-layout") return;
      const theme = data.theme;
      if (theme === "light" || theme === "dark") {
        document.documentElement.dataset.theme = theme;
        document.documentElement.dataset.bsTheme = theme;
      }
      reportSize();
    });
    new ResizeObserver(reportSize).observe(document.body);
    document.addEventListener("load", reportSize, true);
    document.addEventListener("error", reportSize, true);
    reportSize();
    return;
  }

  const frame = document.currentScript.nextElementSibling;
  if (!frame?.matches("iframe.oss-article-sandbox")) return;

  const root = document.documentElement;
  const systemTheme = window.matchMedia("(prefers-color-scheme: dark)");
  function updateFrame() {
    const activeTheme = root.dataset.theme || root.dataset.bsTheme;
    const theme = activeTheme === "light" || activeTheme === "dark" ? activeTheme : systemTheme.matches ? "dark" : "light";
    frame.contentWindow?.postMessage({ type: "oss-article-layout", theme }, "*");
  }

  window.addEventListener("message", (event) => {
    if (event.origin !== "null" || event.source !== frame.contentWindow) return;
    const data = event.data;
    if (data?.type !== "oss-article-height" || !Number.isFinite(data.height) || data.height < 1) return;
    frame.style.height = Math.min(data.height, 1000000) + "px";
  });
  frame.addEventListener("load", updateFrame);
  new MutationObserver(updateFrame).observe(root, { attributes: true, attributeFilter: ["data-theme", "data-bs-theme"] });
  systemTheme.addEventListener("change", updateFrame);
  updateFrame();
})();
