// app.js 提供控制台公共交互：侧边栏折叠、移动抽屉、确认操作、主题切换、CSRF
(function () {
  "use strict";

  document.addEventListener("DOMContentLoaded", function () {
    initSidebar();
    initThemeSwitcher();
    initLanguageSwitcher();
    initConfirmForms();
    initFlashDismiss();
    initCollaborationSelection();
    initThemeSettingGroups();
    initPublicBlogThemeCapability();
    initPluginUploadProgress();
    initPageSizeForms();
    initPluginGuideCopy();
    initPapertrailPreview();
    initServerUpdate();
    initModals();
  });

  function initSidebar() {
    var toggle = document.getElementById("sidebar-toggle");
    var overlay = document.getElementById("sidebar-overlay");
    var closeBtn = document.getElementById("sidebar-close");
    var sidebar = document.getElementById("app-sidebar");

    if (toggle) {
      toggle.addEventListener("click", function () {
        var open = document.body.classList.toggle("drawer-open");
        toggle.setAttribute("aria-expanded", String(open));
      });
    }
    if (closeBtn) {
      closeBtn.addEventListener("click", closeDrawer);
    }
    if (overlay) {
      overlay.addEventListener("click", closeDrawer);
    }
    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") closeDrawer();
    });
    if (sidebar) {
      sidebar.addEventListener("click", function (e) {
        if (e.target.closest("a")) closeDrawer();
      });
    }

    // 二级菜单：一级可折叠项带 aria-expanded 和方向箭头
    document.querySelectorAll(".side-nav__toggle").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var expanded = btn.getAttribute("aria-expanded") === "true";
        btn.setAttribute("aria-expanded", String(!expanded));
        var group = btn.closest(".side-nav__item");
        if (group) group.classList.toggle("is-open", !expanded);
      });
    });

    function setActiveByHash() {
      var accountNavigation = document.querySelector("[data-account-navigation]");
      if (!accountNavigation || normalizePath(window.location.pathname) !== "/dashboard/account") return;

      var section = window.location.hash.substring(1) || "info";
      var next = accountNavigation.querySelector('[data-account-section="' + section + '"]') ||
        accountNavigation.querySelector('[data-account-section="info"]');
      if (!next) return;

      accountNavigation.querySelectorAll(".side-nav__sublink").forEach(function (link) {
        link.classList.toggle("is-active", link === next);
      });

      var group = accountNavigation.closest(".side-nav__item");
      if (!group) return;
      var toggleButton = group.querySelector(".side-nav__toggle");
      group.classList.add("is-open");
      if (toggleButton) toggleButton.setAttribute("aria-expanded", "true");
    }

    function normalizePath(raw) {
      if (!raw) return "";
      return raw.replace(/\/+$/, "") || "/";
    }

    setActiveByHash();
    window.addEventListener("hashchange", setActiveByHash);
  }

  function initPublicBlogThemeCapability() {
    var themeSelect = document.querySelector('select[name="theme_name"]');
    var control = document.querySelector("[data-public-blog-control]");
    var checkbox = control ? control.querySelector('input[name="is_public_blog"]') : null;
    if (!themeSelect || !control || !checkbox) return;
    function update() {
      var option = themeSelect.options[themeSelect.selectedIndex];
      var supported = option && option.dataset.supportsPublicBlog === "true";
      checkbox.disabled = !supported;
      control.title = supported ? "" : (control.dataset.unsupportedTitle || "");
      if (!supported) checkbox.checked = false;
    }
    themeSelect.addEventListener("change", update);
    update();
  }

  function closeDrawer() {
    document.body.classList.remove("drawer-open");
    var toggle = document.getElementById("sidebar-toggle");
    if (toggle) toggle.setAttribute("aria-expanded", "false");
  }

  function initThemeSwitcher() {
    var switcher = document.getElementById("theme-switcher");
    if (!switcher || !window.OSSTheme) return;
    var themeKey = document.documentElement.getAttribute("data-theme-key") || "oss-console-theme";

    function setActive(pref) {
      switcher.querySelectorAll("[data-theme-pref]").forEach(function (btn) {
        var active = btn.getAttribute("data-theme-pref") === pref;
        btn.classList.toggle("is-active", active);
        btn.setAttribute("aria-pressed", String(active));
      });
    }

    setActive(window.OSSTheme.readPreference(themeKey));
    switcher.querySelectorAll("[data-theme-pref]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var pref = btn.getAttribute("data-theme-pref");
        window.OSSTheme.setPreference(themeKey, pref);
        setActive(pref);
      });
    });
  }

  function initLanguageSwitcher() {
    var button = document.querySelector("[data-language-toggle]");
    if (!button) return;

    button.addEventListener("click", function () {
      var target = button.getAttribute("data-language-target");
      if (target !== "zh" && target !== "en") return;

      document.cookie = "oss_web_language=" + target + "; Path=/; Max-Age=31536000; SameSite=Lax";
      button.disabled = true;

      function reloadInLanguage() {
        var next = new URL(window.location.href);
        next.searchParams.set("lang", target);
        window.location.assign(next.toString());
      }

      var csrf = button.getAttribute("data-language-csrf") || "";
      if (!csrf || !window.location.pathname.startsWith("/dashboard")) {
        reloadInLanguage();
        return;
      }

      var body = new URLSearchParams();
      body.set("_csrf", csrf);
      body.set("web_language", target);
      fetch("/dashboard/account/language", {
        method: "POST",
        body: body,
        headers: { "X-CSRF-Token": csrf },
        credentials: "same-origin",
      }).then(reloadInLanguage, reloadInLanguage);
    });
  }

  function initConfirmForms() {
    document.querySelectorAll("form[data-confirm]").forEach(function (form) {
      form.addEventListener("submit", function (e) {
        if (!window.confirm(form.getAttribute("data-confirm"))) {
          e.preventDefault();
        }
      });
    });
  }

  function initFlashDismiss() {
    document.querySelectorAll(".flash .flash__close").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var flash = btn.closest(".flash");
        if (flash) flash.remove();
      });
    });
  }

  function initPluginUploadProgress() {
    document.querySelectorAll("form[data-upload-progress]").forEach(function (form) {
      var bar = form.querySelector("[data-upload-bar]");
      var status = form.querySelector("[data-upload-status]");
      var container = form.querySelector(".upload-progress");
      var submit = form.querySelector('button[type="submit"]');
      var input = form.querySelector('input[type="file"]');
      if (!bar || !status || !submit || !input || !container) return;

      function setProgress(ratio, message) {
        bar.style.width = Math.max(0, Math.min(100, Math.round(ratio * 100))) + "%";
        bar.setAttribute("aria-valuenow", String(Math.round(ratio * 100)));
        status.textContent = message;
      }

      form.addEventListener("submit", function (event) {
        if (!window.XMLHttpRequest || !input.files || input.files.length === 0) return;
        event.preventDefault();
        container.classList.add("is-active");
        var xhr = new XMLHttpRequest();
        var formData = new FormData(form);
        submit.disabled = true;
        setProgress(0, "正在上传 " + input.files[0].name);
        xhr.open(form.getAttribute("method") || "POST", form.getAttribute("action"));
        xhr.upload.addEventListener("progress", function (e) {
          if (!e.lengthComputable) return;
          setProgress(e.loaded / e.total, "上传中 " + Math.round((e.loaded / e.total) * 100) + "%");
        });
        xhr.addEventListener("load", function () {
          setProgress(1, "安装中");
          // XHR 跟随 303 后的 responseURL 即最终落地页；缺失时回到表单目标
          var next = xhr.responseURL || form.getAttribute("action") || window.location.href;
          window.location.assign(next);
        });
        xhr.addEventListener("error", function () {
          submit.disabled = false;
          container.classList.remove("is-active");
          setProgress(0, "上传失败，请重试");
        });
        xhr.send(formData);
      });
    });
  }

  function initPluginGuideCopy() {
    var button = document.querySelector("[data-guide-copy]");
    var source = document.querySelector("script[data-guide-source]");
    if (!button || !source) return;
    button.addEventListener("click", function () {
      var text = source.textContent || "";
      function fallbackCopy() {
        var area = document.createElement("textarea");
        area.value = text;
        area.setAttribute("readonly", "");
        area.style.position = "fixed";
        area.style.opacity = "0";
        document.body.appendChild(area);
        area.select();
        var ok = false;
        try { ok = document.execCommand("copy"); } catch (e) { ok = false; }
        document.body.removeChild(area);
        window.alert(ok ? "插件指南已复制到剪贴板。" : "复制失败，请手动选择文本复制。");
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () {
          window.alert("插件指南已复制到剪贴板。");
        }, fallbackCopy);
        return;
      }
      fallbackCopy();
    });
  }

  function initPageSizeForms() {
    document.querySelectorAll("[data-page-size-form]").forEach(function (form) {
      var select = form.querySelector("[data-page-size-select]");
      if (!select) return;
      // CSP 禁止 inline handler，变更提交统一在这里绑定
      select.addEventListener("change", function () {
        form.submit();
      });
    });
  }

  function initCollaborationSelection() {
    document.querySelectorAll("[data-collaboration-selection]").forEach(function (group) {
      var modal = group.closest(".modal");
      if (!modal) return;
      var checkboxes = group.querySelectorAll('input[name="collaboration_ids"]');
      var submit = modal.querySelector("[data-selection-submit]");
      var selectAll = group.querySelector("[data-select-all]");
      var clearSelection = group.querySelector("[data-clear-selection]");

      function syncSubmit() {
        if (!submit) return;
        submit.disabled = !Array.from(checkboxes).some(function (checkbox) {
          return checkbox.checked;
        });
      }

      if (selectAll) {
        selectAll.addEventListener("click", function () {
          checkboxes.forEach(function (checkbox) { checkbox.checked = true; });
          syncSubmit();
        });
      }
      if (clearSelection) {
        clearSelection.addEventListener("click", function () {
          checkboxes.forEach(function (checkbox) { checkbox.checked = false; });
          syncSubmit();
        });
      }
      checkboxes.forEach(function (checkbox) {
        checkbox.addEventListener("change", syncSubmit);
      });
      syncSubmit();
    });
  }

  function initThemeSettingGroups() {
    document.querySelectorAll("[data-theme-setting-group]").forEach(function (group) {
      var rows = group.querySelector("[data-group-rows]");
      var template = group.querySelector("[data-group-template]");
      var addButton = group.querySelector("[data-group-add]");
      var count = group.querySelector("[data-group-count]");
      var maxItems = Number(group.getAttribute("data-max-items"));
      if (!rows || !template || !addButton || !Number.isInteger(maxItems)) return;

      function notify() {
        rows.dispatchEvent(new CustomEvent("theme-setting-group-changed", {
          bubbles: true,
        }));
      }

      function syncGroup() {
        var rowCount = rows.querySelectorAll("[data-group-row]").length;
        addButton.disabled = rowCount >= maxItems;
        if (count) count.textContent = String(rowCount);
        notify();
      }

      addButton.addEventListener("click", function () {
        if (rows.querySelectorAll("[data-group-row]").length >= maxItems) return;
        var fragment = template.content.cloneNode(true);
        fragment.querySelectorAll("[data-input-name]").forEach(function (input) {
          input.setAttribute("name", input.getAttribute("data-input-name"));
          input.removeAttribute("data-input-name");
        });
        var firstInput = fragment.querySelector("input");
        rows.append(fragment);
        syncGroup();
        if (firstInput) firstInput.focus();
      });

      rows.addEventListener("click", function (event) {
        var removeButton = event.target.closest("[data-group-remove]");
        if (!removeButton) return;
        var row = removeButton.closest("[data-group-row]");
        if (row) row.remove();
        syncGroup();
        addButton.focus();
      });

      syncGroup();
    });
  }

  function initPapertrailPreview() {
    var preview = document.querySelector("[data-papertrail-preview]");
    if (!preview) return;

    var homeLinks = preview.querySelector("[data-papertrail-home-links]");
    var field = function (key) { return document.querySelector('[data-preview-field="' + key + '"]'); };
    var name = field("blog_name");
    var description = field("description");
    var logoURL = field("logo_url");
    var logoSize = field("logo_size");
    var logoShape = field("logo_shape");

    var logoInputs = preview.querySelectorAll("[data-preview-logo]");
    var logoFallback = "";
    var buttonPlaceholder = null;

    if (homeLinks) {
      buttonPlaceholder = homeLinks.querySelector(".papertrail-preview__links-placeholder") || null;
      homeLinks.addEventListener("click", function (event) {
        var anchor = event.target.closest("a");
        if (anchor) {
          event.preventDefault();
        }
      });
    }

    function value(input, fallback) {
      return input && input.value.trim() ? input.value.trim() : fallback;
    }

    function trimText(input) {
      return (input && input.value ? input.value.trim() : "");
    }

    function getLogoSource() {
      var raw = trimText(logoURL);
      if (raw) {
        logoFallback = raw;
        return raw;
      }
      return logoFallback;
    }

    function collectPapertrailButtons() {
      if (!homeLinks) return;
      var group = document.querySelector('[data-theme-setting-group][data-theme-setting-group-key="buttons"]');
      if (!group) return;
      var rows = group.querySelectorAll("[data-group-row]");
      var buttons = [];
      Array.from(rows).forEach(function (row) {
        var label = trimText(row.querySelector('[data-group-key="buttons"][data-group-field-key="label"]'));
        var url = trimText(row.querySelector('[data-group-key="buttons"][data-group-field-key="url"]'));
        var icon = trimText(row.querySelector('[data-group-key="buttons"][data-group-field-key="icon_url"]'));
        if (!label || !url) return;
        buttons.push({
          label: label,
          url: url,
          icon: icon,
        });
      });
      return buttons;
    }

    function renderButtons(buttons) {
      if (!homeLinks) return;
      while (homeLinks.firstChild) {
        homeLinks.removeChild(homeLinks.firstChild);
      }
      if (!buttons || !buttons.length) {
        if (buttonPlaceholder) {
          homeLinks.appendChild(buttonPlaceholder);
        }
        return;
      }
      buttons.forEach(function (button) {
        var item = document.createElement("a");
        item.className = "papertrail-preview__button button";
        item.href = button.url;
        item.setAttribute("aria-label", button.label);
        if (button.icon) {
          var icon = document.createElement("img");
          icon.src = button.icon;
          icon.alt = "";
          icon.width = 16;
          icon.height = 16;
          icon.className = "papertrail-preview__button-icon";
          item.appendChild(icon);
        }
        item.appendChild(document.createTextNode(button.label));
        homeLinks.appendChild(item);
      });
    }

    function refresh() {
      var size = Number(value(logoSize, "96"));
      size = Number.isFinite(size) ? Math.min(192, Math.max(10, Math.round(size))) : 96;
      var circle = value(logoShape, "square") === "circle";
      var source = getLogoSource();
      preview.querySelectorAll("[data-preview-name]").forEach(function (element) {
        element.textContent = value(name, "博客名称");
      });
      preview.querySelectorAll("[data-preview-description]").forEach(function (element) {
        element.textContent = value(description, "博客介绍");
      });
      logoInputs.forEach(function (image) {
        var fallback = image.getAttribute("data-preview-logo-fallback") || "";
        var effectiveSrc = source || fallback;
        if (!effectiveSrc) {
          image.hidden = true;
          return;
        }
        image.src = effectiveSrc;
        image.hidden = false;
        image.style.width = size + "px";
        image.style.height = size + "px";
        image.classList.toggle("is-circle", circle);
      });

      renderButtons(collectPapertrailButtons());
    }

    [name, description, logoURL, logoSize, logoShape].forEach(function (input) {
      if (!input) return;
      input.addEventListener("input", refresh);
      input.addEventListener("change", refresh);
    });
    var buttonGroup = document.querySelector('[data-theme-setting-group][data-theme-setting-group-key="buttons"]');
    if (buttonGroup) {
      var buttonRows = buttonGroup.querySelector("[data-group-rows]");
      if (buttonRows) {
        buttonRows.addEventListener("input", refresh);
        buttonRows.addEventListener("change", refresh);
        buttonRows.addEventListener("theme-setting-group-changed", refresh);
      }
    }

    refresh();
  }

  function initServerUpdate() {
    var panel = document.querySelector("[data-update-panel]");
    if (!panel) return;

    var csrf = panel.getAttribute("data-csrf") || "";
    var checkForm = panel.querySelector("[data-update-check-form]");
    var triggerForm = panel.querySelector("[data-update-trigger-form]");
    var checkBtn = panel.querySelector("[data-update-check-btn]");
    var triggerBtn = panel.querySelector("[data-update-trigger-btn]");
    var checkIdInput = panel.querySelector("[data-update-check-input]");
    var versionInput = panel.querySelector("[data-update-version-input]");
    if (!checkForm || !triggerForm || !checkBtn || !triggerBtn || !checkIdInput || !versionInput) return;

    var latestEl = panel.querySelector("[data-update-latest]");
    var checkIdEl = panel.querySelector("[data-update-check-id]");
    var releaseEl = panel.querySelector("[data-update-release]");
    var expiresEl = panel.querySelector("[data-update-expires]");
    var metaEl = panel.querySelector("[data-update-meta]");
    var noteEl = panel.querySelector("[data-update-note]");
    var noteLineEl = panel.querySelector("[data-update-note-line]");
    var stateEl = panel.querySelector("[data-update-state]");
    var capEl = panel.querySelector("[data-update-capability]");
    var capDetailEl = panel.querySelector("[data-update-capability-detail]");
    var activeIdEl = panel.querySelector("[data-update-active-id]");
    var statusJSONEl = panel.querySelector("[data-update-status-json]");
    var polledEl = panel.querySelector("[data-update-polled]");
    var sourceSelect = panel.querySelector("[data-update-download-source]");
    var customProxyLabel = panel.querySelector("[data-update-custom-proxy]");
    var customProxyInput = panel.querySelector("[data-update-custom-proxy-input]");
    var updateAvailable = false;
    var capabilityReady = panel.getAttribute("data-capability-ready") === "true";
    var externalUpdate = panel.getAttribute("data-external-update") === "true";
    var managedUpdate = panel.getAttribute("data-managed-update") === "true";
    var msg = function (key) { return panel.getAttribute("data-msg-" + key) || ""; };

    function setNote(text) {
      if (noteEl) noteEl.textContent = text;
      if (noteLineEl) noteLineEl.textContent = text;
    }

    function setMetaVisible(visible) {
      if (metaEl) metaEl.classList.toggle("is-visible", visible);
    }

    function setDisabled(disabled) {
      checkBtn.disabled = disabled;
      triggerBtn.disabled = disabled;
      checkBtn.toggleAttribute("aria-disabled", disabled);
      triggerBtn.toggleAttribute("aria-disabled", disabled);
    }

    function syncTrigger() {
      var hidden = !(capabilityReady || managedUpdate) || !updateAvailable || !checkIdInput.value.trim() || !versionInput.value.trim();
      triggerForm.hidden = hidden;
      triggerBtn.disabled = hidden || triggerBtn.hasAttribute("data-busy");
      if (!hidden) triggerBtn.textContent = msg("to-version").replace("{version}", versionInput.value.trim());
    }

    function syncDownloadSource() {
      if (!sourceSelect || !customProxyLabel || !customProxyInput) return;
      var custom = sourceSelect.value === "custom";
      customProxyLabel.hidden = !custom;
      customProxyInput.required = custom;
    }

    function fetchStatus() {
      fetch("/dashboard/admin/system/update/status", { headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" })
        .then(function (response) { return response.json(); })
        .then(function (status) {
          capabilityReady = status.capability_ok === true;
          externalUpdate = status.external_update === true;
          managedUpdate = status.managed_update === true;
          if (stateEl) stateEl.textContent = status.active && status.active.state ? status.active.state : msg("idle");
          if (activeIdEl) activeIdEl.textContent = status.active && status.active.id ? status.active.id : "—";
          if (capEl) capEl.textContent = managedUpdate ? msg("capability-managed-web") : (externalUpdate ? msg("capability-managed") : (capabilityReady ? msg("capability-ok") : msg("capability-not-ready")));
          if (capDetailEl) {
            if (managedUpdate) {
              var mr = status.managed_result;
              capDetailEl.textContent = mr ? (mr.status === "success" ? ("✓ " + (mr.version || "")) : (mr.error || "failed")) : "—";
            } else {
              capDetailEl.textContent = externalUpdate ? msg("external-required") : (status.capability_error || "—");
            }
          }
          if (statusJSONEl) statusJSONEl.textContent = JSON.stringify(status, null, 2);
          if (polledEl) polledEl.textContent = new Date().toLocaleString();
          setDisabled(status.is_updating === true);
          if (!status.is_updating) syncTrigger();
        }).catch(function () {});
    }

    syncTrigger();
    syncDownloadSource();
    if (sourceSelect) sourceSelect.addEventListener("change", syncDownloadSource);
    fetchStatus();
    window.setInterval(fetchStatus, 5000);

    checkBtn.addEventListener("click", function () {
      if (customProxyInput && customProxyInput.required && !customProxyInput.reportValidity()) return;
      updateAvailable = false;
      checkIdInput.value = "";
      versionInput.value = "";
      setNote(msg("checking"));
      setMetaVisible(false);
      syncTrigger();
      checkBtn.disabled = true;
      checkBtn.setAttribute("aria-busy", "true");

      var checkBody = new FormData();
      if (sourceSelect) checkBody.append("download_source", sourceSelect.value);
      if (sourceSelect && sourceSelect.value === "custom" && customProxyInput) checkBody.append("download_proxy", customProxyInput.value.trim());
      fetch(checkForm.getAttribute("data-update-action"), { method: "POST", body: checkBody, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" })
        .then(function (response) { return response.json(); })
        .then(function (result) {
          if (!result.check_id) {
            if (latestEl) latestEl.textContent = result.latest_version || "—";
            setNote(result.note || result.error || "");
            return;
          }

          updateAvailable = result.update_available === true;
          var latestVersion = result.latest_version ? String(result.latest_version).replace(/^v/, "") : (result.candidate && result.candidate.version) || "";
          checkIdInput.value = updateAvailable ? result.check_id : "";
          versionInput.value = updateAvailable ? latestVersion : "";
          if (latestEl) latestEl.textContent = result.latest_version || "—";
          if (checkIdEl) {
            checkIdEl.textContent = result.check_id;
            checkIdEl.setAttribute("title", result.check_id);
          }
          if (releaseEl) {
            releaseEl.textContent = result.release_url || "—";
            if (result.release_url) releaseEl.setAttribute("title", result.release_url);
            else releaseEl.removeAttribute("title");
          }
          if (expiresEl) expiresEl.textContent = result.expires_at ? new Date(result.expires_at / 1e6).toLocaleString() : "—";
          setMetaVisible(true);
          setNote(updateAvailable ? msg(managedUpdate ? "managed-available" : (externalUpdate ? "external-available" : "available")).replace("{version}", latestVersion) : msg("up-to-date"));
        }).catch(function (error) {
          setNote(String(error));
        }).finally(function () {
          checkBtn.disabled = false;
          checkBtn.removeAttribute("aria-busy");
          syncTrigger();
          fetchStatus();
        });
    });

    triggerBtn.addEventListener("click", function () {
      if (customProxyInput && customProxyInput.required && !customProxyInput.reportValidity()) return;
      if (!updateAvailable || !window.confirm(msg("confirm").replace("{version}", versionInput.value.trim()))) return;
      triggerBtn.setAttribute("data-busy", "1");
      triggerBtn.setAttribute("aria-busy", "true");
      triggerBtn.disabled = true;
      var body = new FormData();
      body.append("check_id", checkIdInput.value);
      body.append("expected_version", versionInput.value);
      body.append("confirm", "on");
      if (sourceSelect) body.append("download_source", sourceSelect.value);
      if (sourceSelect && sourceSelect.value === "custom" && customProxyInput) body.append("download_proxy", customProxyInput.value.trim());

      fetch(triggerForm.getAttribute("data-update-action"), { method: "POST", body: body, headers: { "X-CSRF-Token": csrf }, credentials: "same-origin" })
        .then(function (response) { return response.json().then(function (result) { return { status: response.status, body: result }; }); })
        .then(function (result) {
          if (result.status === 202 || result.body.ok) {
            updateAvailable = false;
            setNote(result.body.note || msg(managedUpdate ? "managed-accepted" : "started"));
          } else {
            setNote(result.body.error || result.body.code || "");
          }
        }).catch(function (error) {
          setNote(String(error));
        }).finally(function () {
          triggerBtn.removeAttribute("data-busy");
          triggerBtn.removeAttribute("aria-busy");
          syncTrigger();
          fetchStatus();
        });
    });
  }

  function initModals() {
    var FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

    function getFocusable(modal) {
      var out = [];
      modal.querySelectorAll(FOCUSABLE).forEach(function (el) {
        if (el.offsetParent !== null) out.push(el);
      });
      return out;
    }

    function topModal() {
      var modals = document.querySelectorAll(".modal.is-open");
      return modals.length ? modals[modals.length - 1] : null;
    }

    function closeModal(modal) {
      var trigger = modal.__opener || null;
      modal.classList.remove("is-open");
      if (trigger) {
        trigger.focus();
        trigger.__opener = null;
      }
      modal.__opener = null;
    }

    document.querySelectorAll("[data-modal-open]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var modal = document.getElementById(btn.getAttribute("data-modal-open"));
        if (!modal) return;
        modal.classList.add("is-open");
        btn.__opener = modal;
        modal.__opener = btn;
        var focusable = getFocusable(modal);
        if (focusable.length) focusable[0].focus();
      });
    });

    document.querySelectorAll("[data-modal-close]").forEach(function (el) {
      el.addEventListener("click", function () {
        var modal = el.closest(".modal");
        if (modal) closeModal(modal);
      });
    });

    document.querySelectorAll(".modal__backdrop").forEach(function (backdrop) {
      backdrop.addEventListener("click", function () {
        var modal = backdrop.closest(".modal");
        if (modal) closeModal(modal);
      });
    });

    document.addEventListener("keydown", function (e) {
      if (e.key === "Escape") {
        var modal = topModal();
        if (modal) closeModal(modal);
      }
    });

    document.addEventListener("keydown", function (e) {
      if (e.key !== "Tab") return;
      var modal = topModal();
      if (!modal) return;
      var focusable = getFocusable(modal);
      if (!focusable.length) return;
      var first = focusable[0];
      var last = focusable[focusable.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    });
  }
})();
