// metrics.js 刷新概览和管理员数据页上的实时服务器指标
(function () {
  "use strict";
  var controller;

  function formatBytes(bytes) {
    var units = ["B", "KB", "MB", "GB", "TB"];
    var value = Number(bytes);
    var index = 0;
    while (value >= 1024 && index < units.length - 1) {
      value /= 1024;
      index += 1;
    }
    return (index === 0 ? String(Math.round(value)) : value.toFixed(1)) + " " + units[index];
  }

  function initSystemMetrics() {
    if (controller) controller.abort();
    controller = undefined;
    var container = document.querySelector("[data-system-metrics]");
    if (!container) return;
    var url = container.getAttribute("data-system-metrics-url");
    if (!url) return;
    controller = new AbortController();
    var signal = controller.signal;

    function setMetric(name, value) {
      var element = container.querySelector('[data-system-metric="' + name + '"]');
      if (element) element.textContent = value;
    }

    function refresh() {
      window.fetch(url, { credentials: "same-origin", signal: signal })
        .then(function (response) {
          if (!response.ok) throw new Error("live metrics request failed");
          return response.json();
        })
        .then(function (metrics) {
          setMetric("cpu-model", metrics.cpu_model_name);
          setMetric("cpu-usage", Number(metrics.cpu_usage_percent).toFixed(1) + "%");
          setMetric("memory-usage", Number(metrics.memory_usage_percent).toFixed(1) + "%");
          setMetric("memory-capacity", formatBytes(metrics.memory_used_bytes) + " / " + formatBytes(metrics.memory_total_bytes));
          setMetric("disk-storage", formatBytes(metrics.disk_used_bytes) + " / " + formatBytes(metrics.disk_total_bytes));
          setMetric("project-storage", formatBytes(metrics.project_storage_used) + " / " + (metrics.project_storage_max ? formatBytes(metrics.project_storage_max) : "∞"));
          if (container.querySelector('[data-system-metric="vault-storage"]')) {
            var vaultStorage = formatBytes(metrics.vault_storage_used);
            if (metrics.vault_storage_quota) vaultStorage += " / " + formatBytes(metrics.vault_storage_quota);
            setMetric("vault-storage", vaultStorage);
          }
        })
        .catch(function () {});
    }

    refresh();
    var poll = window.setInterval(refresh, 5000);
    signal.addEventListener("abort", function () { window.clearInterval(poll); }, { once: true });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initSystemMetrics);
  } else {
    initSystemMetrics();
  }
  document.addEventListener("oss:console-page-ready", initSystemMetrics);
})();
