import {
  CATEGORY_LABELS,
  MARKET_INDEX_URL,
  PERMISSION_INFO,
  RUNTIME_LABELS,
  countByCategory,
  formatBytes,
  formatPlatformList,
  normalizeMarketIndex,
  pluginHue,
  pluginMonogram,
  type MarketPlugin,
} from './plugins-market';

function el<K extends keyof HTMLElementTagNameMap>(
  tag: K,
  className?: string,
  text?: string,
): HTMLElementTagNameMap[K] {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function normalizeQuery(text: string): string {
  return text.normalize('NFKC').toLocaleLowerCase().replace(/\s+/g, ' ').trim();
}

function pluginHaystack(plugin: MarketPlugin): string {
  const parts = [
    plugin.name,
    plugin.title,
    plugin.summary,
    plugin.description,
    plugin.kind === 'host' ? 'host 主机 本机' : 'guest 环境内 debian',
    plugin.hasMcp ? 'mcp claude 工具' : '',
    plugin.hasView ? 'view 扩展 界面 页面' : '',
    ...plugin.categories.map(c => `${c} ${CATEGORY_LABELS[c] || ''}`),
    ...plugin.permissions.map(p => `${p} ${PERMISSION_INFO[p]?.title || ''}`),
    plugin.source?.project || '',
    plugin.source?.license || '',
    plugin.author?.name || '',
    plugin.runtime || '',
  ];
  return normalizeQuery(parts.join(' '));
}

function makeIconNode(plugin: MarketPlugin, sizeClass: 'card' | 'detail'): HTMLElement {
  const box = el('span', `pm-icon pm-icon--${sizeClass}`);
  box.setAttribute('aria-hidden', 'true');
  box.style.setProperty('--pm-hue', String(pluginHue(plugin.name)));

  const fallback = () => {
    box.replaceChildren();
    box.classList.add('pm-icon--mono');
    box.textContent = pluginMonogram(plugin.name);
  };

  if (plugin.icon) {
    const img = document.createElement('img');
    img.src = plugin.icon;
    img.alt = '';
    img.loading = 'lazy';
    img.decoding = 'async';
    img.addEventListener('error', fallback, { once: true });
    box.append(img);
  } else {
    fallback();
  }
  return box;
}

function buildCard(plugin: MarketPlugin, onOpen: (plugin: MarketPlugin) => void): HTMLElement {
  const article = el('article', 'pm-card');
  article.dataset.plugin = plugin.name;
  article.dataset.kind = plugin.kind;

  const button = el('button', 'pm-card-trigger');
  button.type = 'button';
  button.setAttribute('aria-label', `查看插件 ${plugin.title} 的详情`);
  button.addEventListener('click', () => onOpen(plugin));

  // Header
  const head = el('div', 'pm-card-head');
  head.append(makeIconNode(plugin, 'card'));

  const heading = el('div', 'pm-card-heading');
  const titleRow = el('div', 'pm-card-title-row');
  titleRow.append(el('h3', undefined, plugin.title));
  titleRow.append(el('span', 'pm-version', `v${plugin.version}`));
  heading.append(titleRow);

  const subRow = el('div', 'pm-card-sub');
  const kindBadge = el(
    'span',
    `pm-pill pm-pill--${plugin.kind}`,
    plugin.kind === 'host' ? '主机插件' : '环境内插件',
  );
  subRow.append(kindBadge);
  if (plugin.hasMcp) subRow.append(el('span', 'pm-pill pm-pill--mcp', 'MCP 工具'));
  if (plugin.hasView) subRow.append(el('span', 'pm-pill pm-pill--view', '界面扩展'));
  heading.append(subRow);

  head.append(heading);
  button.append(head);

  // Description
  button.append(el('p', 'pm-card-desc', plugin.summary || plugin.description));

  // Category & permission preview strip
  const tags = el('div', 'pm-card-tags');
  for (const cat of plugin.categories) {
    tags.append(el('span', 'pm-tag', CATEGORY_LABELS[cat] || cat));
  }
  if (plugin.kind === 'host' && plugin.permissions.length) {
    tags.append(
      el(
        'span',
        'pm-tag pm-tag--perm',
        `需授权：${plugin.permissions.map(p => PERMISSION_INFO[p]?.title || p).join(' · ')}`,
      ),
    );
  } else if (!plugin.permissions.length) {
    tags.append(el('span', 'pm-tag pm-tag--safe', '零越权'));
  }
  button.append(tags);

  // Footer
  const foot = el('div', 'pm-card-foot');
  const meta = el('span', 'pm-card-meta');
  const parts: string[] = [formatPlatformList(plugin.platforms)];
  if (plugin.source?.project) {
    parts.push(plugin.source.license ? `${plugin.source.project} · ${plugin.source.license}` : plugin.source.project);
  } else if (plugin.author?.official) {
    parts.push('Cage 官方');
  } else if (plugin.author?.name) {
    parts.push(plugin.author.name);
  } else if (plugin.minBytes > 0) {
    parts.push(formatBytes(plugin.minBytes));
  }
  meta.textContent = parts.join(' · ');
  foot.append(meta);
  foot.append(el('span', 'pm-card-cta', '查看详情 →'));
  button.append(foot);

  article.append(button);
  return article;
}

function initMarketplace() {
  const dataEl = document.querySelector<HTMLScriptElement>('#plugins-market-data');
  const gridNode = document.querySelector<HTMLElement>('#pm-grid');
  if (!dataEl || !gridNode) return;
  const grid: HTMLElement = gridNode;

  let plugins: MarketPlugin[] = JSON.parse(dataEl.textContent || '[]');
  let activeCategory = 'all';
  let activeKind: 'all' | 'host' | 'guest' = 'all';
  let activeCapability: 'all' | 'mcp' | 'view' = 'all';
  let query = '';

  const searchInput = document.querySelector<HTMLInputElement>('#pm-search');
  const searchClear = document.querySelector<HTMLButtonElement>('#pm-search-clear');
  const categoryBar = document.querySelector<HTMLElement>('#pm-categories');
  const kindButtons = [...document.querySelectorAll<HTMLButtonElement>('[data-filter-kind]')];
  const capButtons = [...document.querySelectorAll<HTMLButtonElement>('[data-filter-cap]')];
  const emptyState = document.querySelector<HTMLElement>('#pm-empty');
  const resetBtn = document.querySelector<HTMLButtonElement>('#pm-reset');
  const countLive = document.querySelector<HTMLElement>('#pm-count');
  const syncStatus = document.querySelector<HTMLElement>('#pm-sync-status');
  const refreshBtn = document.querySelector<HTMLButtonElement>('#pm-refresh');
  const statTotal = document.querySelector<HTMLElement>('#pm-stat-total');
  const statHost = document.querySelector<HTMLElement>('#pm-stat-host');
  const statGuest = document.querySelector<HTMLElement>('#pm-stat-guest');
  const dialog = document.querySelector<HTMLDialogElement>('#pm-dialog');
  const dialogBody = document.querySelector<HTMLElement>('#pm-dialog-body');
  const dialogTitle = document.querySelector<HTMLElement>('#pm-dialog-title');

  function updateHeroStats() {
    if (statTotal) statTotal.textContent = String(plugins.length);
    if (statHost) statHost.textContent = String(plugins.filter(p => p.kind === 'host').length);
    if (statGuest) statGuest.textContent = String(plugins.filter(p => p.kind === 'guest').length);
  }

  function renderCategoryCounts() {
    if (!categoryBar) return;
    const counts = countByCategory(plugins);
    for (const btn of categoryBar.querySelectorAll<HTMLButtonElement>('[data-category]')) {
      const cat = btn.dataset.category || 'all';
      const badge = btn.querySelector<HTMLElement>('.pm-cat-count');
      if (badge) badge.textContent = String(counts[cat] ?? 0);
      btn.setAttribute('aria-pressed', String(cat === activeCategory));
      btn.classList.toggle('is-active', cat === activeCategory);
    }
  }

  function renderFilterStates() {
    for (const btn of kindButtons) {
      const val = btn.dataset.filterKind || 'all';
      btn.setAttribute('aria-pressed', String(val === activeKind));
      btn.classList.toggle('is-active', val === activeKind);
    }
    for (const btn of capButtons) {
      const val = btn.dataset.filterCap || 'all';
      btn.setAttribute('aria-pressed', String(val === activeCapability));
      btn.classList.toggle('is-active', val === activeCapability);
    }
  }

  function openPluginDetail(plugin: MarketPlugin, pushHash = true) {
    if (!dialog || !dialogBody || !dialogTitle) return;
    dialogTitle.textContent = plugin.title;
    dialogBody.replaceChildren();

    // Hero block inside modal
    const hero = el('div', 'pm-detail-hero');
    hero.append(makeIconNode(plugin, 'detail'));

    const heroInfo = el('div', 'pm-detail-hero-info');
    const idRow = el('div', 'pm-detail-id-row');
    idRow.append(el('code', 'pm-slug', plugin.name));
    idRow.append(el('span', 'pm-version', `v${plugin.version}`));
    if (plugin.minBytes > 0) idRow.append(el('span', 'pm-meta-chip', formatBytes(plugin.minBytes)));
    if (plugin.cageEngine) idRow.append(el('span', 'pm-meta-chip', `Cage ${plugin.cageEngine}`));
    if (plugin.runtime) {
      idRow.append(el('span', 'pm-meta-chip', `运行依赖：${RUNTIME_LABELS[plugin.runtime] || plugin.runtime}`));
    }
    heroInfo.append(idRow);

    const pills = el('div', 'pm-card-sub');
    pills.append(
      el(
        'span',
        `pm-pill pm-pill--${plugin.kind}`,
        plugin.kind === 'host' ? '主机插件 · 运行在你的电脑' : '环境内插件 · 运行在 Debian 13',
      ),
    );
    if (plugin.hasMcp) pills.append(el('span', 'pm-pill pm-pill--mcp', 'MCP 工具'));
    if (plugin.hasView) pills.append(el('span', 'pm-pill pm-pill--view', '界面扩展'));
    for (const cat of plugin.categories) {
      pills.append(el('span', 'pm-tag', CATEGORY_LABELS[cat] || cat));
    }
    heroInfo.append(pills);

    if (plugin.author?.official || plugin.author?.name) {
      heroInfo.append(
        el(
          'p',
          'pm-detail-author',
          plugin.author.official ? 'Cage 官方' : plugin.author.name,
        ),
      );
    }

    hero.append(heroInfo);
    dialogBody.append(hero);

    // Descriptions
    const descBox = el('div', 'pm-detail-desc');
    descBox.append(el('p', 'pm-detail-summary', plugin.summary || plugin.description));
    if (plugin.description && plugin.description !== plugin.summary) {
      descBox.append(el('p', 'pm-detail-original', plugin.description));
    }
    dialogBody.append(descBox);

    // Install guide box
    const installSection = el('section', 'pm-detail-install');
    installSection.append(el('h3', undefined, '在 Cage 中一键安装'));
    const steps = el('ol', 'pm-install-steps');
    const step1 = el('li');
    step1.append('打开 Cage 应用，在左侧栏进入 ');
    step1.append(el('strong', undefined, '插件'));
    step1.append(' 并开启总开关。');
    const step2 = el('li');
    step2.append('切换到 ');
    step2.append(el('strong', undefined, '插件市场'));
    step2.append('，搜索 ');
    step2.append(el('code', undefined, plugin.name));
    step2.append(' 点击安装，Cage 会自动下载并校验 SHA-256。');
    const step3 = el('li');
    step3.textContent =
      plugin.kind === 'host'
        ? '首次调用本机工具时会弹出系统确认窗口，你可以选择仅允许本次或「始终允许」。'
        : '安装完成后自动同步到环境内，无需在主机开放权限即可供 Claude 使用。';
    steps.append(step1, step2, step3);
    installSection.append(steps);

    const installActions = el('div', 'pm-detail-actions');
    const repoLink = el('a', 'button pm-btn-sm', '查看插件源码 ↗');
    repoLink.href = plugin.repoUrl;
    repoLink.target = '_blank';
    repoLink.rel = 'noopener noreferrer';
    installActions.append(repoLink);

    const docsLink = el('a', 'text-link', '查看安装与授权说明 →');
    docsLink.href = `${import.meta.env.BASE_URL}docs/plugins/`;
    installActions.append(docsLink);

    installSection.append(installActions);
    dialogBody.append(installSection);

    // Permissions section
    const permSection = el('section', 'pm-detail-section');
    permSection.append(el('h3', undefined, '权限声明与安全边界'));
    if (plugin.permissions.length) {
      const permGrid = el('div', 'pm-perm-grid');
      for (const perm of plugin.permissions) {
        const info = PERMISSION_INFO[perm] || { title: perm, detail: '' };
        const card = el('div', 'pm-perm-item');
        card.append(el('strong', undefined, info.title));
        card.append(el('span', undefined, info.detail));
        permGrid.append(card);
      }
      permSection.append(permGrid);
      permSection.append(
        el(
          'p',
          'pm-detail-note',
          plugin.kind === 'host'
            ? '主机插件以当前系统用户权限运行，每次调用前需经系统确认弹窗放行；若更新新增权限，既有「始终允许」会自动失效。'
            : '环境内插件仅在 Debian 13 隔离环境中运行，不会触及主机系统文件或未共享目录。',
        ),
      );
    } else {
      permSection.append(
        el(
          'p',
          'pm-detail-safe-box',
          '该插件未声明任何越出环境的额外权限。',
        ),
      );
    }
    dialogBody.append(permSection);

    // Contributions (MCP & Views)
    if (plugin.mcpServers.length || plugin.views.length) {
      const contribSection = el('section', 'pm-detail-section');
      contribSection.append(el('h3', undefined, '提供的能力（MCP + 扩展）'));
      const list = el('div', 'pm-contrib-list');
      for (const srv of plugin.mcpServers) {
        const item = el('div', 'pm-contrib-item');
        const label = el('div', 'pm-contrib-head');
        label.append(el('span', 'pm-pill pm-pill--mcp', 'MCP 服务'));
        label.append(el('code', undefined, srv.id));
        item.append(label);
        if (Array.isArray(srv.command) && srv.command.length) {
          item.append(el('pre', 'pm-cmd', srv.command.join(' ')));
        }
        list.append(item);
      }
      for (const view of plugin.views) {
        const item = el('div', 'pm-contrib-item');
        const label = el('div', 'pm-contrib-head');
        label.append(el('span', 'pm-pill pm-pill--view', '侧栏沙箱页面'));
        label.append(el('strong', undefined, view.title || view.id));
        item.append(label);
        item.append(
          el(
            'p',
            'pm-detail-note',
            '页面运行在隔离沙箱框架中，无法联网或访问工作台凭据，只能通过本插件的 MCP 工具读写数据。',
          ),
        );
        list.append(item);
      }
      contribSection.append(list);
      dialogBody.append(contribSection);
    }

    // Upstream open-source project
    if (plugin.source) {
      const srcSection = el('section', 'pm-detail-section');
      srcSection.append(el('h3', undefined, '基于上游开源项目'));
      const box = el('div', 'pm-upstream-box');
      const info = el('div');
      info.append(el('strong', undefined, plugin.source.project));
      const meta = [
        plugin.source.version ? `v${plugin.source.version}` : '',
        plugin.source.license || '',
      ]
        .filter(Boolean)
        .join(' · ');
      if (meta) info.append(el('span', 'pm-upstream-meta', meta));
      box.append(info);
      if (plugin.source.url) {
        const link = el('a', 'text-link', '访问上游仓库 ↗');
        link.href = plugin.source.url;
        link.target = '_blank';
        link.rel = 'noopener noreferrer';
        box.append(link);
      }
      srcSection.append(box);
      dialogBody.append(srcSection);
    }

    // Packages & SHA-256 checksums
    const pkgSection = el('section', 'pm-detail-section');
    pkgSection.append(el('h3', undefined, `安装包与校验（${formatPlatformList(plugin.platforms)}）`));
    const pkgList = el('div', 'pm-pkg-list');
    for (const pkg of plugin.packages) {
      const row = el('div', 'pm-pkg-row');
      const top = el('div', 'pm-pkg-top');
      const labelText = pkg.target || formatPlatformList(pkg.platforms);
      top.append(el('strong', undefined, labelText));
      top.append(el('span', 'pm-meta-chip', formatBytes(pkg.bytes)));
      const dl = el('a', 'text-link pm-pkg-dl', '下载 .tar.gz ↗');
      dl.href = pkg.archive;
      dl.target = '_blank';
      dl.rel = 'noopener noreferrer';
      top.append(dl);
      row.append(top);

      const hashRow = el('div', 'pm-hash-row');
      hashRow.append(el('code', 'pm-hash', `SHA-256: ${pkg.sha256}`));
      const copyBtn = el('button', 'pm-copy-btn', '复制校验值');
      copyBtn.type = 'button';
      copyBtn.addEventListener('click', async () => {
        try {
          await navigator.clipboard.writeText(pkg.sha256);
          copyBtn.textContent = '已复制 ✓';
          window.setTimeout(() => {
            copyBtn.textContent = '复制校验值';
          }, 1800);
        } catch {
          copyBtn.textContent = '复制失败';
        }
      });
      hashRow.append(copyBtn);
      row.append(hashRow);
      pkgList.append(row);
    }
    pkgSection.append(pkgList);
    dialogBody.append(pkgSection);

    if (pushHash) {
      history.replaceState(null, '', `#plugin=${encodeURIComponent(plugin.name)}`);
    }
    if (!dialog.open) dialog.showModal();
  }

  function renderGrid() {
    const tokens = normalizeQuery(query).split(' ').filter(Boolean);
    const filtered = plugins.filter(plugin => {
      if (activeCategory !== 'all' && !plugin.categories.includes(activeCategory)) return false;
      if (activeKind !== 'all' && plugin.kind !== activeKind) return false;
      if (activeCapability === 'mcp' && !plugin.hasMcp) return false;
      if (activeCapability === 'view' && !plugin.hasView) return false;
      if (tokens.length) {
        const hay = pluginHaystack(plugin);
        if (!tokens.every(token => hay.includes(token))) return false;
      }
      return true;
    });

    grid.replaceChildren(...filtered.map(plugin => buildCard(plugin, p => openPluginDetail(p))));
    if (emptyState) emptyState.hidden = filtered.length > 0;
    if (countLive) {
      countLive.textContent =
        filtered.length === plugins.length
          ? `共 ${plugins.length} 款开放插件`
          : `已筛选出 ${filtered.length} / ${plugins.length} 款插件`;
    }
    if (searchClear) searchClear.hidden = !query;
  }

  function resetFilters() {
    activeCategory = 'all';
    activeKind = 'all';
    activeCapability = 'all';
    query = '';
    if (searchInput) searchInput.value = '';
    renderCategoryCounts();
    renderFilterStates();
    renderGrid();
  }

  async function syncLiveIndex(manual = false) {
    if (refreshBtn) {
      refreshBtn.disabled = true;
      refreshBtn.classList.add('is-loading');
    }
    if (manual && syncStatus) syncStatus.textContent = '正在同步 cage-plugins 最新索引…';
    const controller = new AbortController();
    const timer = window.setTimeout(() => controller.abort(), 8000);
    try {
      const response = await fetch(MARKET_INDEX_URL, {
        cache: manual ? 'no-store' : 'default',
        signal: controller.signal,
      });
      if (!response.ok) throw new Error(`HTTP ${response.status}`);
      const json = await response.json();
      const fresh = normalizeMarketIndex(json, import.meta.env.BASE_URL);
      if (fresh.length) {
        plugins = fresh;
        updateHeroStats();
        renderCategoryCounts();
        renderGrid();
        if (syncStatus) syncStatus.textContent = `已同步最新索引 · ${fresh.length} 款插件`;
      }
    } catch {
      if (syncStatus) {
        syncStatus.textContent = manual
          ? '暂时无法连接 GitHub，当前展示构建快照'
          : `展示内置索引快照 · ${plugins.length} 款插件`;
      }
    } finally {
      window.clearTimeout(timer);
      if (refreshBtn) {
        refreshBtn.disabled = false;
        refreshBtn.classList.remove('is-loading');
      }
    }
  }

  function openFromHash() {
    const match = window.location.hash.match(/^#plugin=([a-z0-9-]+)$/i);
    if (!match) return;
    const found = plugins.find(p => p.name === decodeURIComponent(match[1]));
    if (found) openPluginDetail(found, false);
  }

  // Bind controls
  categoryBar?.addEventListener('click', event => {
    const btn = (event.target as HTMLElement).closest<HTMLButtonElement>('[data-category]');
    if (!btn) return;
    activeCategory = btn.dataset.category || 'all';
    renderCategoryCounts();
    renderGrid();
  });

  for (const btn of kindButtons) {
    btn.addEventListener('click', () => {
      const val = (btn.dataset.filterKind || 'all') as 'all' | 'host' | 'guest';
      activeKind = activeKind === val && val !== 'all' ? 'all' : val;
      renderFilterStates();
      renderGrid();
    });
  }

  for (const btn of capButtons) {
    btn.addEventListener('click', () => {
      const val = (btn.dataset.filterCap || 'all') as 'all' | 'mcp' | 'view';
      activeCapability = activeCapability === val && val !== 'all' ? 'all' : val;
      renderFilterStates();
      renderGrid();
    });
  }

  searchInput?.addEventListener('input', () => {
    query = searchInput.value;
    renderGrid();
  });

  searchClear?.addEventListener('click', () => {
    query = '';
    if (searchInput) {
      searchInput.value = '';
      searchInput.focus();
    }
    renderGrid();
  });

  resetBtn?.addEventListener('click', resetFilters);
  refreshBtn?.addEventListener('click', () => void syncLiveIndex(true));

  // Click existing SSR cards (re-rendered cleanly on mount so listeners stay consistent)
  updateHeroStats();
  renderCategoryCounts();
  renderFilterStates();
  renderGrid();

  // Dialog backdrop & close cleanup
  dialog?.addEventListener('click', event => {
    if (event.target !== dialog) return;
    const rect = dialog.getBoundingClientRect();
    if (
      event.clientX < rect.left ||
      event.clientX > rect.right ||
      event.clientY < rect.top ||
      event.clientY > rect.bottom
    ) {
      dialog.close();
    }
  });
  dialog?.addEventListener('close', () => {
    if (window.location.hash.startsWith('#plugin=')) {
      history.replaceState(null, '', window.location.pathname + window.location.search);
    }
  });

  // Keyboard shortcuts: "/" focuses search
  document.addEventListener('keydown', event => {
    if (
      event.key === '/' &&
      !event.metaKey &&
      !event.ctrlKey &&
      !event.altKey &&
      document.activeElement?.tagName !== 'INPUT' &&
      document.activeElement?.tagName !== 'TEXTAREA' &&
      !dialog?.open
    ) {
      event.preventDefault();
      searchInput?.focus();
    }
  });

  window.addEventListener('hashchange', openFromHash);
  openFromHash();
  void syncLiveIndex(false);
}

initMarketplace();
