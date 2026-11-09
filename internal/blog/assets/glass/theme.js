(() => {
  "use strict";

  const clamp = (value, min, max) => {
    if (value < min) return min;
    if (value > max) return max;
    return value;
  };

  const getCopyMessageArea = () => {
    const status = document.querySelector("[data-copy-status]");
    if (!status) return () => {};
    return (message) => {
      status.textContent = message;
    };
  };

  const initThemeSwitcher = () => {
    const switcher = document.getElementById("theme-switcher");
    if (!switcher || !window.OSSTheme) return;
    const themeKey = document.documentElement.dataset.themeKey || "oss-blog-theme";
    const setActive = (preference) => {
      switcher.querySelectorAll("[data-theme-pref]").forEach((button) => {
        const active = button.dataset.themePref === preference;
        button.classList.toggle("is-active", active);
        button.setAttribute("aria-pressed", String(active));
      });
    };
    setActive(window.OSSTheme.readPreference(themeKey));
    switcher.addEventListener("click", (event) => {
      const button = event.target.closest("[data-theme-pref]");
      if (!button) return;
      window.OSSTheme.setPreference(themeKey, button.dataset.themePref);
      setActive(button.dataset.themePref);
    });
  };

  const initTableOfContents = () => {
    const toc = document.querySelector("[data-reading-toc]");
    const content = document.querySelector("[data-reading-content]");
    if (!toc || !content) return;
    const list = toc.querySelector("[data-toc-list]");
    const headings = Array.from(content.querySelectorAll("h1:not([data-article-title]), h2, h3"));
    if (!list || headings.length === 0) return;
    const usedIDs = new Set();
    content.querySelectorAll("[id]").forEach((element) => {
      if (!/^H[1-3]$/.test(element.tagName)) usedIDs.add(element.id);
    });
    const items = [];
    headings.forEach((heading, index) => {
      const originalID = heading.id;
      let id = originalID || `section-${index + 1}`;
      let suffix = 2;
      while (usedIDs.has(id)) {
        id = `${originalID || `section-${index + 1}`}-${suffix}`;
        suffix += 1;
      }
      heading.id = id;
      usedIDs.add(id);
      const item = document.createElement("li");
      item.dataset.level = heading.tagName.slice(1);
      const link = document.createElement("a");
      link.href = `#${id}`;
      link.textContent = heading.textContent.trim();
      link.setAttribute("aria-label", `跳转到 ${link.textContent}`);
      item.append(link);
      list.append(item);
      items.push({ heading, link });
    });

    content.querySelectorAll("a[data-local-heading]").forEach((link) => {
      const target = link.dataset.localHeading;
      const heading = headings.find((candidate) => candidate.textContent.trim() === target);
      if (heading && heading.id) link.href = `#${heading.id}`;
    });

    const setActive = (next) => {
      list.querySelectorAll("a").forEach((anchor) => {
        const active = anchor === next;
        anchor.classList.toggle("is-active", active);
        if (active) {
          anchor.setAttribute("aria-current", "location");
        } else {
          anchor.removeAttribute("aria-current");
        }
      });
    };

    const isLastAnchor = () => {
      return window.scrollY + window.innerHeight >= document.documentElement.scrollHeight - 12;
    };

    const panel = toc.querySelector("[data-toc-panel]") || toc;
    const openButton = toc.querySelector("[data-toc-open]");
    const closeButton = toc.querySelector("[data-toc-close]");
    const backdrop = toc.querySelector("[data-toc-backdrop]");
    const MOBILE_TOC = "(max-width: 780px)";
    const isMobileDrawer = () => window.matchMedia(MOBILE_TOC).matches;
    const getDrawerFocusables = () => {
      return Array.from(panel.querySelectorAll('a[href], button:not([disabled]), [tabindex]:not([tabindex="-1"])')).filter((candidate) => {
        return candidate instanceof HTMLElement && candidate.offsetParent !== null;
      });
    };
    const lockBodyScroll = () => {
      document.body.setAttribute("data-reading-toc-open", "1");
      document.body.style.overflow = "hidden";
    };
    const unlockBodyScroll = () => {
      document.body.removeAttribute("data-reading-toc-open");
      document.body.style.overflow = "";
    };
    const setDrawerState = (isOpen) => {
      toc.classList.toggle("is-open", isOpen);
      if (openButton) {
        openButton.setAttribute("aria-expanded", String(isOpen));
      }
      if (isOpen && backdrop) {
        backdrop.hidden = false;
      }
      if (backdrop) {
        backdrop.setAttribute("aria-hidden", String(!isOpen));
      }
    };
    const restoreFocus = () => {
      if (openButton instanceof HTMLElement) {
        openButton.focus();
      }
    };
    const focusDrawer = () => {
      const focusables = getDrawerFocusables();
      (focusables[0] || panel).focus?.();
    };
    const openDrawer = () => {
      if (!isMobileDrawer() || toc.classList.contains("is-open")) return;
      setDrawerState(true);
      lockBodyScroll();
      document.addEventListener("keydown", onDrawerKeyDown);
      requestAnimationFrame(() => {
        focusDrawer();
      });
    };
    const closeDrawer = () => {
      if (!toc.classList.contains("is-open")) return;
      setDrawerState(false);
      unlockBodyScroll();
      document.removeEventListener("keydown", onDrawerKeyDown);
      restoreFocus();
    };
    const onDrawerKeyDown = (event) => {
      if (event.key === "Escape") {
        closeDrawer();
        return;
      }
      if (event.key !== "Tab") return;
      const focusables = getDrawerFocusables();
      if (focusables.length === 0) {
        event.preventDefault();
        panel.focus?.();
        return;
      }
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    const onMediaOrResize = () => {
      if (!isMobileDrawer()) {
        closeDrawer();
      }
    };

    const refreshOnScroll = () => {
      let active = items[items.length - 1]?.link;
      if (!active) return;
      if (!isLastAnchor()) {
        const viewportY = window.scrollY + 108;
        for (let i = 0; i < items.length; i++) {
          const heading = items[i].heading;
          if (heading.offsetTop > viewportY) {
            active = i === 0 ? items[0].link : items[i - 1].link;
            break;
          }
        }
      }
      setActive(active);
    };

    let raf;
    const onScroll = () => {
      if (raf) return;
      raf = window.requestAnimationFrame(() => {
        raf = null;
        refreshOnScroll();
      });
    };

    setActive(items[0]?.link);
    if (openButton) {
      openButton.setAttribute("aria-expanded", "false");
    }
    if (backdrop) {
      backdrop.hidden = true;
      backdrop.setAttribute("aria-hidden", "true");
    }
    if (toc.id) {
      openButton?.setAttribute("aria-controls", toc.id);
    } else if (panel) {
      const panelId = panel.id || "reading-toc-panel";
      panel.id = panelId;
      openButton?.setAttribute("aria-controls", panelId);
    }
    if (openButton) {
      openButton.addEventListener("click", openDrawer);
    }
    if (closeButton) {
      closeButton.addEventListener("click", closeDrawer);
    }
    if (backdrop) {
      backdrop.addEventListener("click", closeDrawer);
    }
    list.addEventListener("click", (event) => {
      if (!event.target.closest("a")) return;
      if (isMobileDrawer()) closeDrawer();
    });
    const mediaQuery = window.matchMedia(MOBILE_TOC);
    if (mediaQuery.addEventListener) {
      mediaQuery.addEventListener("change", onMediaOrResize);
    } else {
      mediaQuery.addListener(onMediaOrResize);
    }
    window.addEventListener("resize", onMediaOrResize);
    window.addEventListener("scroll", onScroll, { passive: true });
    refreshOnScroll();
    toc.parentElement.classList.add("with-reading-toc");
    toc.hidden = false;
  };

  const initCopy = () => {
    const button = document.querySelector("[data-copy-article]");
    const content = document.querySelector("[data-reading-content]");
    if (!button || !content) return;
    const label = button.querySelector("[data-copy-label]");
    const setStatus = getCopyMessageArea();
    button.addEventListener("click", async () => {
      try {
        const copy = content.cloneNode(true);
        copy.querySelectorAll("[data-copy-article], .code-copy").forEach((control) => control.remove());
        await navigator.clipboard.writeText(copy.innerText.trim());
        if (label) label.textContent = "已复制";
        button.classList.add("is-success");
        setStatus("文章已复制到剪贴板。");
        const original = button.getAttribute("aria-label") || "一键复制";
        button.setAttribute("aria-label", "文章已复制");
        window.setTimeout(() => {
          if (label) label.textContent = "一键复制";
          button.classList.remove("is-success");
          button.setAttribute("aria-label", original);
        }, 1600);
      } catch (error) {
        setStatus("复制失败，请检查浏览器剪贴板权限。");
      }
    });
  };

  const initCodeCopy = () => {
    const content = document.querySelector("[data-reading-content]");
    if (!content || !content.hasAttribute("data-allow-copy")) return;
    const setStatus = getCopyMessageArea();
    content.querySelectorAll("pre").forEach((block) => {
      const code = block.querySelector("code");
      const text = (code ? code.textContent : block.textContent).trim();
      if (!text) return;
      const button = document.createElement("button");
      button.type = "button";
      button.className = "code-copy";
      button.textContent = "复制代码";
      button.setAttribute("aria-label", "复制代码块");
      button.addEventListener("click", async () => {
        try {
          await navigator.clipboard.writeText(text);
          button.textContent = "已复制";
          button.classList.add("is-success");
          setStatus("代码已复制到剪贴板。");
          window.setTimeout(() => {
            button.textContent = "复制代码";
            button.classList.remove("is-success");
          }, 1600);
        } catch (error) {
          setStatus("复制失败，请检查浏览器剪贴板权限。");
        }
      });
      const wrapper = document.createElement("div");
      wrapper.className = "code-block";
      block.parentNode.insertBefore(wrapper, block);
      const viewport = document.createElement("div");
      viewport.className = "code-viewport";
      wrapper.append(viewport);
      viewport.append(block);
      wrapper.insertBefore(button, viewport);
    });
  };

  const initBackToTop = () => {
    const button = document.querySelector("[data-back-to-top]");
    if (!button) return;
    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    button.addEventListener("click", () => {
      window.scrollTo({ top: 0, behavior: reducedMotion ? "auto" : "smooth" });
    });
  };

  const initLogoSizing = () => {
    const logo = document.querySelector("[data-theme-logo]");
    if (!logo) return;
    const value = Number(logo.getAttribute("width"));
    if (!Number.isFinite(value)) return;
    const size = clamp(Math.round(value), 16, 192);
    logo.style.width = `${size}px`;
    logo.style.height = `${size}px`;
    logo.classList.add("is-sized");
  };

  const initThemePicker = () => {
    const select = document.querySelector("[data-theme-preview]");
    if (!select) return;
    select.addEventListener("change", () => {
      const url = new URL(window.location.href);
      url.searchParams.set("theme", select.value);
      window.location.href = url.toString();
    });
  };

  const initReveal = () => {
    const items = document.querySelectorAll("[data-reveal]");
    if (items.length === 0) return;
    const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (reducedMotion || !("IntersectionObserver" in window)) {
      items.forEach((item) => item.setAttribute("data-reveal-ready", ""));
      return;
    }
    const observer = new IntersectionObserver((entries) => {
      entries.forEach((entry) => {
        if (!entry.isIntersecting) return;
        entry.target.setAttribute("data-reveal-ready", "");
        observer.unobserve(entry.target);
      });
    }, { rootMargin: "0px 0px -8% 0px", threshold: 0.08 });
    items.forEach((item) => observer.observe(item));
  };

  const disableMissingThemeScript = () => {
    if (!window.OSSTheme) {
      document.documentElement.classList.remove("oss-theme-ready");
    }
  };

  document.addEventListener("DOMContentLoaded", () => {
    document.documentElement.classList.add("oss-theme-ready");
    disableMissingThemeScript();
    initThemeSwitcher();
    initTableOfContents();
    initCopy();
    initCodeCopy();
    initBackToTop();
    initLogoSizing();
    initThemePicker();
    initReveal();
  });
})();
