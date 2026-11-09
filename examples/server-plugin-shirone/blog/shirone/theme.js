(() => {
  'use strict';

  const STYLES = ['tonalSpot', 'vibrant', 'content', 'expressive', 'rainbow', 'fruitSalad', 'monochrome', 'neutral', 'fidelity'];
  const TEXTURES = ['none', 'starlight', 'cyber-dots', 'topography', 'geometric', 'sakura'];
  const DEFAULTS = { scheme: 'auto', hue: 315, style: 'tonalSpot', spec: '2025', wallpaper: 'banner', layout: 'list', texture: 'starlight', textureOpacity: 0.12, reducedMotion: false };
  const clamp = (value, min, max) => Math.min(max, Math.max(min, value));
  const numeric = (value, fallback, min, max) => value !== '' && value !== null && Number.isFinite(Number(value)) ? clamp(Number(value), min, max) : fallback;
  const oneOf = (value, values, fallback) => values.includes(value) ? value : fallback;
  const normalizeSettings = (value) => {
    const input = value && typeof value === 'object' ? value : {};
    return {
      scheme: oneOf(input.scheme, ['auto', 'light', 'dark'], DEFAULTS.scheme),
      hue: Math.round(numeric(input.hue, DEFAULTS.hue, 0, 360)),
      style: oneOf(input.style, STYLES, DEFAULTS.style),
      spec: oneOf(input.spec, ['2021', '2025'], DEFAULTS.spec),
      wallpaper: oneOf(input.wallpaper, ['banner', 'none'], DEFAULTS.wallpaper),
      layout: oneOf(input.layout, ['list', 'grid'], DEFAULTS.layout),
      texture: oneOf(input.texture, TEXTURES, DEFAULTS.texture),
      textureOpacity: numeric(input.textureOpacity, DEFAULTS.textureOpacity, 0, 1),
      reducedMotion: input.reducedMotion === true,
    };
  };
  const fold = (value) => String(value || '').normalize('NFKC').toLocaleLowerCase();
  const matchesPost = (post, filters) => {
    if (filters.category && post.category !== filters.category) return false;
    if (filters.tag && !post.tags.includes(filters.tag)) return false;
    if (filters.date && post.date !== filters.date) return false;
    const haystack = fold([post.title, post.summary, post.category, ...post.tags].join(' '));
    return fold(filters.query).trim().split(/\s+/).filter(Boolean).every((term) => haystack.includes(term));
  };
  const paginate = (items, requestedPage, pageSize = 8) => {
    const size = Math.max(1, Math.floor(Number(pageSize)) || 8);
    const pages = Math.max(1, Math.ceil(items.length / size));
    const page = clamp(Math.floor(Number(requestedPage)) || 1, 1, pages);
    return { page, pages, items: items.slice((page - 1) * size, page * size) };
  };
  const dayKey = (date) => `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, '0')}-${String(date.getDate()).padStart(2, '0')}`;
  const monthDays = (year, month) => {
    const first = new Date(year, month, 1);
    const days = new Date(year, month + 1, 0).getDate();
    return [...Array((first.getDay() + 6) % 7).fill(null), ...Array.from({ length: days }, (_, index) => dayKey(new Date(year, month, index + 1)))];
  };

  const sameOriginURL = (value, baseURL) => {
    if (typeof value !== 'string' || !value.trim()) return null;
    try {
      const base = new URL(baseURL);
      const url = new URL(value, base);
      return /^https?:$/.test(url.protocol) && url.origin === base.origin && !url.username && !url.password ? url : null;
    } catch (_) { return null; }
  };
  const readIndexPost = (element, baseURL) => {
    const url = sameOriginURL(element.getAttribute('href'), baseURL);
    if (!url || !/^\/p\/[^/]+$/.test(url.pathname) || url.search || url.hash) return null;
    return {
      title: element.dataset.title || '', summary: element.dataset.summary || '',
      category: element.dataset.category || '',
      tags: [...new Set(Array.from(element.querySelectorAll('[data-tag]')).map((tag) => tag.dataset.tag).filter(Boolean))],
      date: (element.dataset.date || '').slice(0, 10),
      words: Number.isFinite(Number(element.dataset.words)) ? Math.max(0, Number(element.dataset.words)) : 0, url: url.href,
    };
  };
  const readHomeIndex = (page, context, baseURL) => {
    const identity = page.body?.dataset;
    if (!context.vaultId || !context.themeName || identity?.page !== 'home' || identity.vaultId !== context.vaultId || identity.themeName !== context.themeName) throw new Error('博客首页标记不匹配');
    const index = page.querySelector('#post-index');
    if (!index) throw new Error('博客首页没有文章索引');
    const seen = new Set();
    const posts = Array.from(index.querySelectorAll('a')).map((element) => {
      const post = readIndexPost(element, baseURL);
      if (!post) throw new Error('博客索引包含无效文章链接');
      return post;
    }).filter((post) => {
      if (seen.has(post.url)) return false;
      seen.add(post.url); return true;
    });
    return {
      status: 'ready', posts,
      name: page.querySelector('[data-blog-name]')?.textContent.trim() || '',
      description: page.querySelector('[data-blog-description]')?.textContent.trim() || '',
    };
  };
  const loadHomeIndex = async (context, fetchPage, parseHTML) => {
    if (!context.homeURL) return { status: 'disabled', posts: [] };
    const home = sameOriginURL(context.homeURL, context.pageURL);
    if (!home) return { status: 'error', posts: [] };
    const abort = new AbortController();
    const timeout = setTimeout(() => abort.abort(), 8000);
    try {
      const response = await fetchPage(home.href, { credentials: 'same-origin', mode: 'same-origin', redirect: 'error', cache: 'no-store', signal: abort.signal, headers: { Accept: 'text/html' } });
      const finalURL = sameOriginURL(response.url, home.href);
      if (!response.ok || !finalURL || finalURL.href !== home.href || !/^text\/html(?:;|$)/i.test(response.headers.get('content-type') || '')) throw new Error('博客首页不可用');
      return readHomeIndex(parseHTML(await response.text()), context, finalURL.href);
    } catch (_) { return { status: 'error', posts: [] }; }
    finally { clearTimeout(timeout); }
  };

  if (typeof module !== 'undefined' && module.exports) module.exports = { normalizeSettings, matchesPost, paginate, monthDays, sameOriginURL, readIndexPost, readHomeIndex, loadHomeIndex };
  if (typeof document === 'undefined') return;

  const $ = (selector, parent = document) => parent.querySelector(selector);
  const $$ = (selector, parent = document) => Array.from(parent.querySelectorAll(selector));
  const root = document.documentElement;
  const themeScriptURL = document.currentScript?.src || $$('script[src]').find((script) => /\/theme\.js(?:[?#]|$)/.test(script.src))?.src;
  const mediaDark = window.matchMedia('(prefers-color-scheme: dark)');
  const mediaMotion = window.matchMedia('(prefers-reduced-motion: reduce)');
  const storageKey = `oss-blog-shirone:${document.body.dataset.vaultId || 'public'}`;
  let settings = { ...DEFAULTS };
  try { settings = normalizeSettings(JSON.parse(localStorage.getItem(storageKey) || 'null')); } catch (_) { /* 浏览器禁用存储时使用站点默认设置 */ }
  const reducedMotion = () => settings.reducedMotion || mediaMotion.matches;
  const scrollBehavior = () => reducedMotion() ? 'instant' : 'smooth';
  const make = (tag, className, text) => {
    const element = document.createElement(tag);
    if (className) element.className = className;
    if (text !== undefined) element.textContent = text;
    return element;
  };
  const button = (className, text, label) => {
    const element = make('button', className, text);
    element.type = 'button';
    if (label) element.setAttribute('aria-label', label);
    return element;
  };
  const announce = (message) => { const area = $('#copy-status'); if (area) area.textContent = message; };
  const localURL = (href) => sameOriginURL(href, location.href);
  const updateTopbar = () => {
    const bar = $('.topbar'); const banner = $('.banner');
    if (!bar) return;
    const bounds = banner?.getBoundingClientRect();
    bar.classList.toggle('is-over-banner', Boolean(bounds && bounds.height > 0 && bounds.bottom > 64));
    bar.classList.toggle('is-scrolled', window.scrollY > 24);
  };
  const colorCache = new Map();
  const colors = (style = settings.style) => {
    if (!window.ShironeColor) return null;
    const key = [settings.hue, root.classList.contains('dark'), style, settings.spec].join(':');
    if (!colorCache.has(key)) {
      if (colorCache.size > 120) colorCache.clear();
      colorCache.set(key, window.ShironeColor.resolveScheme(settings.hue, root.classList.contains('dark'), style, settings.spec));
    }
    return colorCache.get(key);
  };
  const applySettings = (persist = false) => {
    root.classList.toggle('dark', settings.scheme === 'dark' || (settings.scheme === 'auto' && mediaDark.matches));
    root.classList.toggle('motion-reduced', reducedMotion());
    root.dataset.reducedMotion = String(reducedMotion());
    root.dataset.wallpaper = settings.wallpaper;
    root.dataset.layout = settings.layout;
    root.dataset.texturePreset = settings.texture;
    root.dataset.scheme = settings.scheme;
    root.dataset.spec = settings.spec;
    root.dataset.style = settings.style;
    root.style.setProperty('--hue', String(settings.hue));
    root.style.setProperty('--texture-opacity', String(settings.textureOpacity));
    root.style.colorScheme = root.classList.contains('dark') ? 'dark' : 'light';
    updateTopbar();
    const scheme = colors();
    if (scheme) Object.entries(scheme).forEach(([role, value]) => {
      const property = `--mc-${role.replace(/[A-Z]/g, (letter) => `-${letter.toLowerCase()}`)}`;
      if (value) root.style.setProperty(property, value); else root.style.removeProperty(property);
    });
    $$('[data-setting]').forEach((control) => {
      const name = control.dataset.setting;
      if (!Object.hasOwn(settings, name)) return;
      if (control.type === 'radio') {
        control.checked = control.value === String(settings[name]);
        control.closest('label')?.classList.toggle('is-selected', control.checked);
      }
      else if (control.type === 'checkbox') control.checked = settings[name] === true;
      else if (control.tagName === 'BUTTON') {
        const selected = control.value === String(settings[name]);
        control.classList.toggle('is-active', selected);
        control.setAttribute('aria-pressed', String(selected));
      } else control.value = settings[name];
    });
    const hueOutput = $('#hue-value'); if (hueOutput) hueOutput.textContent = String(settings.hue);
    $$('[data-action="cycle-scheme"]').forEach((control) => {
      const label = `明暗模式：${{auto: '自动', light: '浅色', dark: '深色'}[settings.scheme]}，点击切换`;
      control.setAttribute('aria-label', label); control.title = label; control.dataset.scheme = settings.scheme;
    });
    $$('[data-setting-output]').forEach((output) => {
      const name = output.dataset.settingOutput;
      output.textContent = name === 'textureOpacity' ? `${Math.round(settings.textureOpacity * 100)}%` : String(settings[name] ?? '');
    });
    $$('.color-styles label').forEach((label) => {
      const input = $('[data-setting="style"]', label);
      if (!input) return;
      let preview = $('[data-style-preview]', label);
      if (!preview) {
        preview = make('span', 'color-preview'); preview.dataset.stylePreview = input.value; preview.setAttribute('aria-hidden', 'true');
        ['primary', 'secondary', 'tertiary'].forEach((role) => { const dot = make('span'); dot.style.backgroundColor = `var(--preview-${role})`; preview.append(dot); });
        label.prepend(preview);
      }
    });
    $$('[data-style-preview]').forEach((preview) => {
      const palette = colors(preview.dataset.stylePreview);
      if (!palette) return;
      ['primary', 'secondary', 'tertiary'].forEach((role) => preview.style.setProperty(`--preview-${role}`, palette[role]));
    });
    if (persist) { try { localStorage.setItem(storageKey, JSON.stringify(settings)); } catch (_) { /* 隐私模式下偏好仅在当前页面有效 */ } }
  };

  const dialogOpeners = new WeakMap();
  const openDialog = (id, opener) => {
    const dialog = document.getElementById(id);
    if (!dialog || dialog.open) return;
    dialogOpeners.set(dialog, opener || document.activeElement);
    dialog.showModal();
    document.body.classList.add('dialog-open');
    if (id === 'search-dialog') { renderSearch(); $('#search-input')?.focus(); }
  };
  const closeDialog = (dialog) => { if (dialog?.open) dialog.close(); };
  const initDialogs = () => {
    $$('dialog').forEach((dialog) => {
      dialog.addEventListener('click', (event) => {
        if (event.target !== dialog) return;
        const box = dialog.getBoundingClientRect();
        if (event.clientX < box.left || event.clientX > box.right || event.clientY < box.top || event.clientY > box.bottom) closeDialog(dialog);
      });
      dialog.addEventListener('close', () => {
        if (!$('dialog[open]')) document.body.classList.remove('dialog-open');
        const opener = dialogOpeners.get(dialog);
        if (opener?.isConnected) opener.focus({ preventScroll: true });
      });
    });
    $('#mobile-menu')?.addEventListener('click', (event) => { if (event.target.closest('a[href]')) closeDialog($('#mobile-menu')); });
  };

  const postFrom = (element) => ({
    element,
    title: element.dataset.title || $('.post-title', element)?.textContent.trim() || element.textContent.trim(),
    summary: element.dataset.summary || '',
    category: element.dataset.category || '',
    tags: [...new Set($$('[data-tag]', element).map((tag) => tag.dataset.tag).filter(Boolean))],
    date: (element.dataset.date || '').slice(0, 10),
    words: Math.max(0, Number(element.dataset.words) || 0),
    url: localURL(element.getAttribute('href') || $('a.post-link', element)?.getAttribute('href') || '')?.href || '',
  });
  const indexContext = { homeURL: document.body.dataset.homeUrl || '', pageURL: location.href, vaultId: document.body.dataset.vaultId, themeName: document.body.dataset.themeName };
  let posts = [];
  let indexStatus = indexContext.homeURL ? 'loading' : 'disabled';
  const cards = $$('#post-list .post-card').map(postFrom);
  const filters = { category: '', tag: '', date: '', query: '' };
  const initialQuery = new URLSearchParams(location.search);
  ['category', 'tag', 'date'].forEach((key) => { filters[key] = initialQuery.get(key) || ''; });
  let currentPage = Number(initialQuery.get('page')) || 1;
  let currentPost;
  const grouped = (key) => {
    const counts = new Map();
    posts.forEach((post) => (key === 'tags' ? post.tags : [post[key]]).filter(Boolean).forEach((value) => counts.set(value, (counts.get(value) || 0) + 1)));
    return [...counts].sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0], 'zh-CN'));
  };
  let categories = [];
  let tags = [];
  const updateFilterURL = () => {
    const url = new URL(location.href);
    ['category', 'tag', 'date'].forEach((key) => { if (filters[key]) url.searchParams.set(key, filters[key]); else url.searchParams.delete(key); });
    if (currentPage > 1) url.searchParams.set('page', String(currentPage)); else url.searchParams.delete('page');
    try { history.replaceState(null, '', url); } catch (_) { /* 嵌入环境可禁止修改地址，筛选仍在当前页面生效 */ }
  };
  const applyFilter = (key, value) => {
    if (!$('#post-list')) {
      const url = localURL(indexContext.homeURL);
      if (url) { if (value) url.searchParams.set(key, value); location.assign(url.href); }
      return;
    }
    filters[key] = filters[key] === value ? '' : value;
    currentPage = 1;
    renderPosts(true);
    renderCalendar();
  };
  const initPostWidgets = () => {
    $$('[data-post-index-widget]').forEach((widget) => { widget.hidden = indexStatus !== 'ready'; });
    if (indexStatus !== 'ready') {
      ['#category-list', '#tag-list', '#category-bar'].forEach((selector) => $(selector)?.replaceChildren());
      $$('#stat-posts, #stat-categories, #stat-tags, #stat-words').forEach((element) => { element.textContent = '—'; });
      return;
    }
    [['#category-list', categories, 'category'], ['#category-bar', categories, 'category'], ['#tag-list', tags, 'tag']].forEach(([selector, values, key]) => {
      const container = $(selector);
      if (!container) return;
      container.replaceChildren();
      if (!values.length) { const widget = container.closest('[data-widget], .widget'); if (widget) widget.hidden = true; else container.hidden = true; return; }
      container.hidden = false;
      if (selector === '#category-bar') {
        const all = button('chip is-active', '全部');
        all.dataset.category = '';
        container.append(all);
      }
      values.forEach(([value, count]) => {
        const chip = button('chip', undefined, `${value}，${count} 篇文章`);
        chip.dataset[key] = value;
        chip.append(make('span', '', value), make('span', 'chip-count', String(count)));
        container.append(chip);
      });
    });
    [['#stat-posts', posts.length], ['#stat-categories', categories.length], ['#stat-tags', tags.length], ['#stat-words', posts.reduce((sum, post) => sum + post.words, 0)]].forEach(([selector, value]) => {
      const element = $(selector); if (element) element.textContent = Number(value).toLocaleString('zh-CN');
    });
  };
  const renderPosts = (updateURL = false) => {
    if (!$('#post-list')) return;
    const matching = cards.filter((post) => matchesPost(post, filters));
    const result = paginate(matching, currentPage);
    currentPage = result.page;
    const visible = new Set(result.items);
    cards.forEach((post) => { post.element.hidden = !visible.has(post); });
    const empty = $('#post-empty');
    if (empty) empty.hidden = matching.length !== 0;
    const filterStatus = $('#filter-status');
    const active = [filters.category && `分类：${filters.category}`, filters.tag && `标签：${filters.tag}`, filters.date && `日期：${filters.date}`].filter(Boolean);
    if (filterStatus) { filterStatus.textContent = active.length ? `${active.join(' · ')} · ${matching.length} 篇文章` : `共 ${matching.length} 篇文章`; }
    const filterBar = $('#clear-filter')?.closest('.filter-bar');
    if (filterBar) filterBar.hidden = active.length === 0;
    $$('button[data-category], button[data-tag]').forEach((control) => {
      const key = control.hasAttribute('data-category') ? 'category' : 'tag';
      const selected = filters[key] === control.dataset[key];
      control.classList.toggle('is-active', selected);
      control.setAttribute('aria-pressed', String(selected));
    });
    const pagination = $('#pagination');
    if (pagination) {
      pagination.replaceChildren();
      pagination.hidden = result.pages <= 1;
      const pageButton = (label, page, disabled, ariaLabel) => {
        const control = button('page-button', label, ariaLabel);
        control.disabled = disabled;
        if (page === result.page && /^\d+$/.test(label)) { control.classList.add('is-active'); control.setAttribute('aria-current', 'page'); }
        control.addEventListener('click', () => { currentPage = page; renderPosts(true); $('#post-list').scrollIntoView({ behavior: scrollBehavior(), block: 'start' }); });
        pagination.append(control);
      };
      pageButton('‹', result.page - 1, result.page === 1, '上一页');
      let previous = 0;
      for (let page = 1; page <= result.pages; page++) {
        if (page !== 1 && page !== result.pages && Math.abs(page - result.page) > 1) continue;
        if (previous && page - previous > 1) pagination.append(make('span', 'pagination-gap', '…'));
        pageButton(String(page), page, false, `第 ${page} 页`);
        previous = page;
      }
      pageButton('›', result.page + 1, result.page === result.pages, '下一页');
    }
    if (updateURL) updateFilterURL();
  };
  const renderSearch = () => {
    const results = $('#search-results');
    if (!results) return;
    const query = $('#search-input')?.value.trim() || '';
    results.replaceChildren();
    if (indexStatus !== 'ready') {
      const message = indexStatus === 'loading' ? '正在加载文章列表…' : indexStatus === 'disabled' ? '此分享没有公开博客索引。' : '文章列表暂时无法加载。';
      const status = make('p', 'search-status', message); status.setAttribute('role', 'status'); results.append(status);
      const home = localURL(indexContext.homeURL);
      if (indexStatus === 'error' && home) { const link = make('a', 'search-result', '返回博客首页查看文章'); link.href = home.href; results.append(link); }
      return;
    }
    const matches = posts.filter((post) => matchesPost(post, { query }));
    const status = make('p', 'search-status', query ? `找到 ${matches.length} 篇文章` : `共 ${matches.length} 篇文章，输入标题、摘要、标签或分类搜索`);
    status.setAttribute('role', 'status');
    results.append(status);
    matches.forEach((post) => {
      const link = make('a', 'search-result');
      link.href = post.url;
      link.append(make('strong', 'search-result-title', post.title));
      if (post.summary) link.append(make('p', 'search-result-summary', post.summary));
      link.append(make('span', 'search-result-meta', [post.date, post.category, post.tags.join(' · ')].filter(Boolean).join(' · ')));
      results.append(link);
    });
    if (!matches.length) results.append(make('p', 'search-empty', '没有找到匹配的文章，请尝试其他关键词。'));
  };

  const today = new Date();
  let calendarDate = new Date(today.getFullYear(), today.getMonth(), 1);
  let activeMonths = [];
  const adjacentMonth = (direction) => {
    const current = dayKey(calendarDate).slice(0, 7);
    return direction < 0 ? activeMonths.filter((month) => month < current).at(-1) : activeMonths.find((month) => month > current);
  };
  const shiftCalendar = (direction) => {
    const target = adjacentMonth(direction);
    if (target) { const [year, month] = target.split('-').map(Number); calendarDate = new Date(year, month - 1, 1); }
    else if (!activeMonths.length) calendarDate = new Date(calendarDate.getFullYear(), calendarDate.getMonth() + direction, 1);
    renderCalendar();
  };
  const renderCalendar = () => {
    const grid = $('#calendar-grid');
    if (!grid) return;
    if (indexStatus !== 'ready') { grid.replaceChildren(); return; }
    const label = $('#calendar-label');
    if (label) label.textContent = new Intl.DateTimeFormat('zh-CN', { year: 'numeric', month: 'long' }).format(calendarDate);
    grid.replaceChildren();
    monthDays(calendarDate.getFullYear(), calendarDate.getMonth()).forEach((date) => {
      if (!date) { const blank = make('span', 'calendar-blank'); blank.setAttribute('aria-hidden', 'true'); grid.append(blank); return; }
      const count = posts.filter((post) => post.date === date).length;
      const control = button('calendar-day', String(Number(date.slice(-2))), `${date}${count ? `，${count} 篇文章` : ''}`);
      control.disabled = !count;
      control.classList.toggle('has-posts', count > 0);
      control.classList.toggle('is-today', date === dayKey(today));
      control.classList.toggle('is-selected', filters.date === date);
      control.setAttribute('aria-pressed', String(filters.date === date));
      if (date === dayKey(today)) control.setAttribute('aria-current', 'date');
      control.addEventListener('click', () => applyFilter('date', date));
      grid.append(control);
    });
    [['calendar-prev', -1], ['calendar-next', 1]].forEach(([action, direction]) => { const control = $(`[data-action="${action}"]`); if (control) control.disabled = activeMonths.length > 0 && !adjacentMonth(direction); });
  };

  const initArticleLinks = () => {
    const related = $('#related-posts');
    const navigation = $('#post-navigation');
    [related, navigation].forEach((element) => { if (element) { element.replaceChildren(); element.hidden = !currentPost; } });
    if (!currentPost) return;
    if (related) {
      const ranked = posts.filter((post) => post !== currentPost).map((post) => ({ post, score: Number(Boolean(post.category) && post.category === currentPost.category) * 2 + post.tags.filter((tag) => currentPost.tags.includes(tag)).length })).filter((entry) => entry.score > 0).sort((a, b) => b.score - a.score).slice(0, 3);
      ranked.forEach(({ post }) => { const link = make('a', 'related-post', post.title); link.href = post.url; related.append(link); });
      if (!ranked.length) { const section = related.closest('[data-widget]'); if (section) section.hidden = true; else related.hidden = true; }
    }
    if (navigation) {
      const index = posts.indexOf(currentPost);
      [[posts[index - 1], '上一篇', 'previous'], [posts[index + 1], '下一篇', 'next']].forEach(([post, label, direction]) => {
        if (!post) return;
        const link = make('a', `post-navigation-link ${direction}`);
        link.href = post.url;
        link.append(make('span', 'post-navigation-label', label), make('strong', '', post.title));
        navigation.append(link);
      });
    }
  };
  const refreshIndex = (result) => {
    posts = result.status === 'ready' ? result.posts : [];
    indexStatus = result.status;
    categories = grouped('category'); tags = grouped('tags');
    currentPost = posts.find((post) => new URL(post.url).pathname === location.pathname);
    activeMonths = [...new Set(posts.map((post) => post.date.slice(0, 7)).filter((date) => /^\d{4}-\d{2}$/.test(date)))].sort();
    if (result.status === 'ready') {
      if (document.body.dataset.blogNameEmpty === 'true' && result.name) $$('[data-blog-name]').forEach((element) => { element.textContent = result.name; });
      if (result.description) $$('[data-blog-description]').filter((element) => !element.textContent.trim()).forEach((element) => { element.textContent = result.description; element.hidden = false; });
    }
    initPostWidgets(); renderPosts(); renderCalendar(); initArticleLinks();
    if ($('#search-dialog')?.open) renderSearch();
  };
  const initIndex = () => {
    if (document.body.dataset.page === 'home') {
      try { refreshIndex(readHomeIndex(document, indexContext, location.href)); } catch (_) { refreshIndex({ status: 'error', posts: [] }); }
      return;
    }
    refreshIndex({ status: indexStatus, posts: [] });
    loadHomeIndex(indexContext, (url, options) => fetch(url, options), (html) => new DOMParser().parseFromString(html, 'text/html')).then(refreshIndex);
  };
  const initReading = () => {
    const content = $('#article-content');
    const tocContainer = $('#toc');
    const toc = tocContainer ? make('ul', 'toc-list') : null;
    if (tocContainer) tocContainer.replaceChildren(toc);
    const tocCard = $('#toc-card');
    const tocHome = tocCard?.parentElement;
    const compactTOC = window.matchMedia('(max-width: 1279px)');
    const placeTOC = () => {
      if (!tocCard || !tocHome) return;
      const article = $('.article-card');
      const details = $('details', tocCard);
      if (compactTOC.matches && article) { article.before(tocCard); if (details) details.open = false; }
      else { tocHome.prepend(tocCard); if (details) details.open = true; }
    };
    placeTOC(); compactTOC.addEventListener('change', placeTOC);
    const headings = content ? $$('h1,h2,h3,h4,h5,h6', content) : [];
    const used = new Set($$('[id]').filter((element) => !headings.includes(element)).map((element) => element.id));
    const items = [];
    headings.forEach((heading, index) => {
      const seed = heading.id || `section-${index + 1}`;
      let id = seed; let suffix = 2;
      while (used.has(id)) id = `${seed}-${suffix++}`;
      heading.id = id; used.add(id);
      if (!toc) return;
      const item = make('li'); item.dataset.level = heading.tagName.slice(1);
      const link = make('a', '', heading.textContent.trim()); link.href = `#${encodeURIComponent(id)}`;
      link.addEventListener('click', (event) => {
        event.preventDefault();
        heading.scrollIntoView({ behavior: scrollBehavior(), block: 'start' });
        heading.setAttribute('tabindex', '-1'); heading.focus({ preventScroll: true });
        try { history.replaceState(null, '', `#${encodeURIComponent(id)}`); } catch (_) { /* 沙箱页面可能禁止历史记录写入 */ }
      });
      item.append(link); toc.append(item); items.push({ heading, link });
    });
    if (tocCard) tocCard.hidden = !headings.length;
    if (content) $$('a[data-local-heading]', content).forEach((link) => { const heading = headings.find((item) => item.textContent.trim() === link.dataset.localHeading); if (heading) link.href = `#${encodeURIComponent(heading.id)}`; });
    let scheduled = false;
    const update = () => {
      scheduled = false;
      updateTopbar();
      const atBottom = window.scrollY + window.innerHeight >= root.scrollHeight - 8;
      const scrollPadding = Number.parseFloat(getComputedStyle(root).scrollPaddingTop) || 0;
      let active = items[0];
      for (const item of items) {
        const scrollMargin = Number.parseFloat(getComputedStyle(item.heading).scrollMarginTop) || 0;
        if (item.heading.getBoundingClientRect().top <= Math.max(88, scrollPadding + scrollMargin) + 2 || atBottom) active = item;
      }
      items.forEach((item) => {
        const selected = item === active;
        item.link.classList.toggle('is-active', selected);
        if (selected) item.link.setAttribute('aria-current', 'location'); else item.link.removeAttribute('aria-current');
      });
      if (content) {
        const rectangle = content.getBoundingClientRect();
        const span = rectangle.height - window.innerHeight + 120;
        const progress = span <= 0 ? (rectangle.top < window.innerHeight ? 100 : 0) : clamp((120 - rectangle.top) / span * 100, 0, 100);
        const bar = $('#reading-progress');
        if (bar) { bar.style.width = `${progress}%`; bar.setAttribute('aria-valuenow', String(Math.round(progress))); }
        root.style.setProperty('--reading-progress', `${progress}%`);
      }
      $$('[data-action="back-top"]').forEach((control) => { const visible = window.scrollY > 260; control.hidden = !visible; control.classList.toggle('is-visible', visible); });
    };
    const schedule = () => { if (!scheduled) { scheduled = true; requestAnimationFrame(update); } };
    window.addEventListener('scroll', schedule, { passive: true });
    window.addEventListener('resize', schedule);
    window.addEventListener('pageshow', schedule);
    if (content && window.ResizeObserver) new ResizeObserver(schedule).observe(content);
    const restoreHash = () => {
      let id;
      try { id = decodeURIComponent(location.hash.slice(1)); } catch (_) { return; }
      const heading = headings.find((item) => item.id === id);
      if (heading) { heading.scrollIntoView({ behavior: 'instant', block: 'start' }); update(); }
    };
    window.addEventListener('hashchange', restoreHash);
    if (location.hash) {
      requestAnimationFrame(restoreHash);
      if (document.readyState !== 'complete') window.addEventListener('load', restoreHash, { once: true });
    }
    update();
  };
  const copyLabels = new WeakMap();
  const copyText = async (text, control, success) => {
    try {
      await navigator.clipboard.writeText(text);
      announce(success);
      const label = control ? ($('[data-copy-label]', control) || (!$('svg', control) ? control : null)) : null;
      if (label) {
        if (!copyLabels.has(label)) copyLabels.set(label, label.textContent);
        label.textContent = '已复制';
        window.setTimeout(() => { if (label.isConnected) label.textContent = copyLabels.get(label); }, 1600);
      }
      if (control) { control.classList.add('is-success'); window.setTimeout(() => control.classList.remove('is-success'), 1600); }
    } catch (_) { announce('复制失败，请检查浏览器的剪贴板权限。'); }
  };
  const allowsCopy = () => { const value = $('#article-content')?.getAttribute('data-allow-copy'); return value === '' || value === 'true'; };
  const initSyntax = () => {
    const blocks = $$('#article-content pre code');
    if (!blocks.length || !themeScriptURL) return;
    const highlight = () => {
      if (typeof window.ShironeHighlight !== 'function') return;
      blocks.forEach((code) => { try { window.ShironeHighlight(code); } catch (_) { /* 无法识别的代码保留文本显示 */ } });
    };
    if (window.ShironeHighlight) { highlight(); return; }
    const script = make('script');
    script.src = new URL('syntax.js', themeScriptURL).href;
    script.onload = highlight;
    script.setAttribute('data-cfasync', 'false');
    document.head.append(script);
  };
  const initCodeCopy = () => {
    const content = $('#article-content');
    if (!content || !allowsCopy()) return;
    $$('pre', content).forEach((pre) => {
      const code = $('code', pre);
      const text = (code || pre).textContent;
      const wrapper = make('div', 'code-block');
      const control = button('code-copy', '复制代码', '复制代码块');
      control.addEventListener('click', () => copyText(text, control, '代码已复制到剪贴板。'));
      pre.before(wrapper); wrapper.append(control, pre);
    });
  };

  let imageIndex = 0;
  let imageScale = 1;
  const articleImages = $$('#article-content img');
  const imageDialog = $('#image-dialog');
  const viewerImage = imageDialog ? $('img', imageDialog) : null;
  const showImage = (index) => {
    if (!viewerImage || !articleImages.length) return;
    imageIndex = (index + articleImages.length) % articleImages.length;
    imageScale = 1;
    const original = articleImages[imageIndex];
    viewerImage.src = original.currentSrc || original.src;
    viewerImage.alt = original.alt || '';
    viewerImage.style.transform = '';
    viewerImage.classList.remove('is-zoomed');
    const zoom = $('[data-image-zoom]'); if (zoom) { zoom.setAttribute('aria-pressed', 'false'); zoom.textContent = '放大'; }
    const caption = $('#image-caption'); if (caption) caption.textContent = `${original.alt || '文章图片'} · ${imageIndex + 1} / ${articleImages.length}`;
  };
  const zoomImage = () => {
    if (!viewerImage) return;
    imageScale = imageScale === 1 ? 2 : 1;
    viewerImage.style.transform = imageScale === 1 ? '' : 'scale(2)';
    viewerImage.classList.toggle('is-zoomed', imageScale > 1);
    const control = $('[data-image-zoom]'); if (control) { control.setAttribute('aria-pressed', String(imageScale > 1)); control.textContent = imageScale > 1 ? '缩小' : '放大'; }
  };
  const initImages = () => {
    if (!imageDialog || !viewerImage || !articleImages.length) return;
    const tools = make('div', 'image-tools');
    const previous = button('icon-button', '←', '上一张图片');
    const next = button('icon-button', '→', '下一张图片');
    const zoom = button('icon-button', '放大', '切换图片缩放'); zoom.dataset.imageZoom = ''; zoom.setAttribute('aria-pressed', 'false');
    previous.hidden = next.hidden = articleImages.length < 2;
    previous.addEventListener('click', () => showImage(imageIndex - 1));
    next.addEventListener('click', () => showImage(imageIndex + 1));
    zoom.addEventListener('click', zoomImage);
    tools.append(previous, zoom, next); imageDialog.append(tools);
    viewerImage.addEventListener('dblclick', zoomImage);
    imageDialog.addEventListener('keydown', (event) => {
      if (event.key === 'ArrowLeft') { event.preventDefault(); showImage(imageIndex - 1); }
      if (event.key === 'ArrowRight') { event.preventDefault(); showImage(imageIndex + 1); }
    });
    articleImages.forEach((img, index) => {
      const anchor = img.closest('a[href]');
      if (anchor) {
        if (anchor.href !== new URL(img.src, location.href).href) return;
      }
      img.tabIndex = 0;
      img.setAttribute('role', 'button');
      img.setAttribute('aria-label', `查看图片：${img.alt || `第 ${index + 1} 张图片`}`);
      const open = (event) => { event.preventDefault(); showImage(index); openDialog('image-dialog', img); };
      img.addEventListener('click', open);
      img.addEventListener('keydown', (event) => { if (event.key === 'Enter' || event.key === ' ') open(event); });
    });
  };
  const handleAction = (event) => {
    const control = event.target.closest('[data-action]');
    if (!control) return;
    switch (control.dataset.action) {
      case 'open-settings': openDialog('settings-dialog', control); break;
      case 'cycle-scheme': settings.scheme = ['auto', 'light', 'dark'][(['auto', 'light', 'dark'].indexOf(settings.scheme) + 1) % 3]; applySettings(true); break;
      case 'open-search': openDialog('search-dialog', control); break;
      case 'open-menu': openDialog('mobile-menu', control); break;
      case 'close-dialog': closeDialog(control.closest('dialog')); break;
      case 'reset-settings': settings = { ...DEFAULTS }; applySettings(true); announce('显示设置已恢复默认。'); break;
      case 'back-top': window.scrollTo({ top: 0, behavior: scrollBehavior() }); break;
      case 'calendar-prev': shiftCalendar(-1); break;
      case 'calendar-next': shiftCalendar(1); break;
      case 'calendar-today': calendarDate = new Date(today.getFullYear(), today.getMonth(), 1); renderCalendar(); break;
      case 'copy-link': copyText(location.href, control, '文章链接已复制到剪贴板。'); break;
      case 'copy-article': {
        if (!allowsCopy()) break;
        const clone = $('#article-content').cloneNode(true);
        $$('.code-copy', clone).forEach((item) => item.remove());
        $$('br', clone).forEach((item) => item.replaceWith(document.createTextNode('\n')));
        $$('p,pre,li,h1,h2,h3,h4,h5,h6,blockquote,tr', clone).forEach((item) => item.append(document.createTextNode('\n')));
        copyText(clone.textContent.trim(), control, '文章已复制到剪贴板。');
        break;
      }
    }
  };
  const init = () => {
    root.classList.add('shirone-ready');
    applySettings();
    initDialogs(); initIndex(); initReading(); initSyntax(); initCodeCopy(); initImages();
    document.addEventListener('click', handleAction);
    document.addEventListener('click', (event) => {
      const control = event.target.closest('button[data-category], button[data-tag]');
      if (!control) return;
      const key = control.hasAttribute('data-category') ? 'category' : 'tag';
      applyFilter(key, control.dataset[key]);
    });
    document.addEventListener('input', (event) => {
      const control = event.target.closest('[data-setting]');
      if (!control) return;
      settings = normalizeSettings({ ...settings, [control.dataset.setting]: control.type === 'checkbox' ? control.checked : control.value });
      applySettings(true);
    });
    document.addEventListener('click', (event) => {
      const control = event.target.closest('button[data-setting]');
      if (!control) return;
      settings = normalizeSettings({ ...settings, [control.dataset.setting]: control.value }); applySettings(true);
    });
    $('#search-input')?.addEventListener('input', renderSearch);
    $('#clear-filter')?.addEventListener('click', () => { filters.category = filters.tag = filters.date = ''; currentPage = 1; renderPosts(true); renderCalendar(); });
    $('#calendar-label')?.addEventListener('click', () => { calendarDate = new Date(today.getFullYear(), today.getMonth(), 1); renderCalendar(); });
    document.addEventListener('keydown', (event) => {
      if (indexContext.homeURL && (event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') { event.preventDefault(); openDialog('search-dialog', document.activeElement); }
    });
    mediaDark.addEventListener('change', () => applySettings());
    mediaMotion.addEventListener('change', () => applySettings());
    window.addEventListener('storage', (event) => { if (event.key === storageKey) { try { settings = normalizeSettings(JSON.parse(event.newValue)); applySettings(); } catch (_) { /* 不使用其他页面写入的无效偏好 */ } } });
  };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init, { once: true }); else init();
})();
