export const MARKET_INDEX_URL = 'https://raw.githubusercontent.com/Lee-xeo/cage-plugins/main/index.json';
export const MARKET_ARCHIVE_PREFIX = 'https://raw.githubusercontent.com/Lee-xeo/cage-plugins/main/dist/';
export const MARKET_ICON_PREFIX = 'https://raw.githubusercontent.com/Lee-xeo/cage-plugins/main/plugins/';
export const MARKET_REPO_URL = 'https://github.com/Lee-xeo/cage-plugins';

export interface RawMarketSource {
  project: string;
  version?: string;
  license?: string;
  url?: string;
}

export interface RawMarketAuthor {
  name: string;
  official?: boolean;
}

export interface RawMcpServer {
  id: string;
  command: string[];
}

export interface RawPluginView {
  id: string;
  title?: string;
  entry?: string;
  server?: string;
  openUrls?: string[];
}

export interface RawContributes {
  mcpServers?: RawMcpServer[];
  views?: RawPluginView[];
}

export interface RawLocalizedText {
  displayName?: string;
  description?: string;
}

export interface RawMarketEntry {
  name: string;
  displayName?: string;
  version: string;
  description: string;
  author?: RawMarketAuthor;
  i18n?: Record<string, RawLocalizedText>;
  platforms?: string[];
  permissions?: string[];
  categories?: string[];
  kind?: string;
  engines?: { cage?: string };
  contributes?: RawContributes;
  target?: string;
  runtime?: string;
  archive: string;
  sha256: string;
  bytes: number;
  icon?: string;
  source?: RawMarketSource;
}

export interface RawMarketIndex {
  version: number;
  plugins: RawMarketEntry[];
}

export interface PluginPackage {
  target: string;
  platforms: string[];
  archive: string;
  sha256: string;
  bytes: number;
}

export interface MarketPlugin {
  name: string;
  title: string;
  version: string;
  description: string;
  summary: string;
  kind: 'host' | 'guest';
  categories: string[];
  permissions: string[];
  platforms: string[];
  hasMcp: boolean;
  hasView: boolean;
  mcpServers: RawMcpServer[];
  views: RawPluginView[];
  runtime: string;
  cageEngine: string;
  icon: string;
  author?: RawMarketAuthor;
  source?: RawMarketSource;
  packages: PluginPackage[];
  minBytes: number;
  repoUrl: string;
}

export const CATEGORY_ORDER = [
  'desktop',
  'productivity',
  'developer',
  'files',
  'network',
  'system',
  'example',
] as const;

export const CATEGORY_LABELS: Record<string, string> = {
  desktop: '桌面控制',
  productivity: '效率',
  developer: '开发',
  files: '文件',
  network: '网络',
  system: '系统与状态',
  example: '示例',
};

export const PERMISSION_INFO: Record<string, { title: string; detail: string }> = {
  files: { title: '读写文件', detail: '读取或修改这台电脑上的文件。' },
  commands: { title: '运行命令', detail: '以你的系统账户在这台电脑上运行命令。' },
  screen: { title: '查看屏幕', detail: '截取或读取屏幕上显示的内容。' },
  input: { title: '控制键盘和鼠标', detail: '模拟键盘输入和鼠标操作。' },
  network: { title: '访问网络', detail: '从当前运行位置连接网络上的服务。' },
};

export const PLATFORM_LABELS: Record<string, string> = {
  darwin: 'macOS',
  windows: 'Windows',
  linux: 'Linux',
};

export const RUNTIME_LABELS: Record<string, string> = {
  python3: 'Python 3',
  uv: 'uv / Python',
  npx: 'Node.js / npx',
  node: 'Node.js',
};

const KNOWN_ZH_SUMMARIES: Record<string, string> = {
  blender: '连接本机正在运行的 Blender：读取场景、创建与修改模型材质、执行 Python 脚本并截取视口画面。',
  browser: '在环境内驱动无头 Chromium 打开网页、读取无障碍结构快照，并完成点击、输入、填表与截图。',
  clock: '读取当前电脑的本机时间，适合作为最小主机 MCP 插件示例。',
  'desktop-control': '查看电脑屏幕并控制本机键鼠，让环境里的 Claude 经你授权后直接操作桌面应用。',
  'documents-to-markdown': '将 PDF、Word、Excel、PowerPoint、EPUB 与 HTML 文档解析、大纲化并检索为干净的 Markdown。',
  'excel-workbooks': '创建、读取和编辑 Excel 工作簿（.xlsx），支持公式、格式、图表、透视表与工作表，无需安装 Office。',
  'git-repository': '读取与管理 Git 仓库：查看状态、差异、提交历史、分支，以及暂存和提交代码。',
  'knowledge-memory': '维护跨对话持久化的本地知识图谱，记录实体、关系与观察笔记。',
  'linux-desktop': '查看 Cage 内 Debian 桌面并控制其键鼠，直接协作操作环境内的图形程序。',
  'local-preview': '打开本机回环地址（localhost）上的开发预览页面，并读回标签页标题与当前地址。',
  'powerpoint-slides': '创建、读取和编辑 PowerPoint 演示文稿（.pptx），支持幻灯片、表格、图表、图片与模板，无需安装 Office。',
  sqlite: '查询与管理本地 SQLite 数据库：执行读写 SQL、建表、检查表结构并记录分析笔记。',
  'system-status': '同时提供侧栏可视化页面与 MCP 工具，实时查看 Cage 环境的 CPU 负载、内存与磁盘占用。',
  'web-fetch': '抓取网页并将 HTML 转换为整洁的 Markdown 供 Claude 阅读，支持分页与 robots.txt。',
  'word-documents': '创建、读取和编辑 Word 文档（.docx），支持正文、标题、表格、图片、样式与批注，无需安装 Office。',
};

const PLUGIN_NAME_RE = /^[a-z0-9](?:[a-z0-9-]{0,38}[a-z0-9])?$/;
const SHA256_RE = /^[a-f0-9]{64}$/i;
const LICENSE_RE = /^[A-Za-z0-9.+-]+( (AND|OR|WITH) [A-Za-z0-9.+-]+)*$/;
const ICON_EXT_RE = /\.(?:svg|png)$/i;

function isSafeHttpsUrl(raw: string | undefined): boolean {
  if (!raw || raw.length > 300) return false;
  try {
    const parsed = new URL(raw);
    return parsed.protocol === 'https:' && Boolean(parsed.hostname) && !parsed.username && !parsed.password && !parsed.port;
  } catch {
    return false;
  }
}

function sanitizeSource(source: RawMarketSource | undefined): RawMarketSource | undefined {
  if (!source || typeof source.project !== 'string') return undefined;
  const project = source.project.trim();
  if (!project || project.length > 100) return undefined;
  const version = typeof source.version === 'string' && source.version.length <= 40 ? source.version.trim() : undefined;
  const license =
    typeof source.license === 'string' && source.license.length <= 60 && LICENSE_RE.test(source.license.trim())
      ? source.license.trim()
      : undefined;
  const url = isSafeHttpsUrl(source.url) ? source.url!.trim() : undefined;
  return { project, version, license, url };
}

function sanitizeAuthor(author: RawMarketAuthor | undefined): RawMarketAuthor | undefined {
  if (!author || typeof author.name !== 'string') return undefined;
  const name = author.name.trim();
  if (!name || name.length > 80) return undefined;
  return { name, official: Boolean(author.official) };
}

function localizedZhText(i18n: Record<string, RawLocalizedText> | undefined): RawLocalizedText | undefined {
  if (!i18n || typeof i18n !== 'object') return undefined;
  const candidate = i18n['zh-CN'] || i18n['zh-cn'] || i18n['zh'];
  if (!candidate || typeof candidate !== 'object') return undefined;
  const displayName = typeof candidate.displayName === 'string' ? candidate.displayName.trim().slice(0, 80) : '';
  const description = typeof candidate.description === 'string' ? candidate.description.trim().slice(0, 600) : '';
  if (!displayName && !description) return undefined;
  return { displayName: displayName || undefined, description: description || undefined };
}

const LOCAL_PLUGIN_ICONS = new Set([
  'blender',
  'browser',
  'clock',
  'desktop-control',
  'documents-to-markdown',
  'excel-workbooks',
  'git-repository',
  'knowledge-memory',
  'linux-desktop',
  'local-preview',
  'powerpoint-slides',
  'sqlite',
  'system-status',
  'web-fetch',
  'word-documents',
]);

function sanitizeIcon(name: string, icon: string | undefined, baseUrl: string): string {
  if (LOCAL_PLUGIN_ICONS.has(name)) return `${baseUrl}plugins-icons/${name}.svg`;
  if (!icon || typeof icon !== 'string') return '';
  const prefix = `${MARKET_ICON_PREFIX}${name}/`;
  if (!icon.startsWith(prefix)) return '';
  const rest = icon.slice(prefix.length);
  if (!rest || /[?#%\\/]/.test(rest) || !ICON_EXT_RE.test(rest)) return '';
  return icon;
}

function fallbackTitle(name: string): string {
  return name
    .split(/[-_\s]+/)
    .filter(Boolean)
    .map(word => word[0].toUpperCase() + word.slice(1))
    .join(' ');
}

export function formatBytes(bytes: number): string {
  if (!bytes || bytes <= 0) return '';
  if (bytes < 1024 * 1024) return `${Math.max(1, Math.round(bytes / 1024))} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

export function formatPlatformList(platforms: string[]): string {
  if (!platforms.length) return '全平台';
  const ordered = ['darwin', 'windows', 'linux'].filter(p => platforms.includes(p));
  if (ordered.length === 3) return '全平台';
  return ordered.map(p => PLATFORM_LABELS[p] || p).join(' · ');
}

export function pluginMonogram(name: string): string {
  const parts = name.split(/[-_\s]+/).filter(Boolean);
  return parts.slice(0, 2).map(w => w[0].toUpperCase()).join('') || '?';
}

export function pluginHue(name: string): number {
  let value = 17;
  for (const ch of name) value = (value * 31 + (ch.codePointAt(0) ?? 0)) % 360;
  return value;
}

export function normalizeMarketIndex(raw: unknown, baseUrl = '/'): MarketPlugin[] {
  if (!raw || typeof raw !== 'object') return [];
  const doc = raw as Partial<RawMarketIndex>;
  if (doc.version !== 1 || !Array.isArray(doc.plugins)) return [];

  const grouped = new Map<string, MarketPlugin>();

  for (const entry of doc.plugins) {
    if (!entry || typeof entry !== 'object') continue;
    if (typeof entry.name !== 'string' || !PLUGIN_NAME_RE.test(entry.name)) continue;
    if (typeof entry.version !== 'string' || !entry.version || entry.version.length > 40) continue;
    if (typeof entry.archive !== 'string' || !entry.archive.startsWith(MARKET_ARCHIVE_PREFIX)) continue;
    if (typeof entry.sha256 !== 'string' || !SHA256_RE.test(entry.sha256)) continue;
    if (typeof entry.bytes !== 'number' || entry.bytes <= 0) continue;

    const kind: 'host' | 'guest' = entry.kind === 'guest' ? 'guest' : 'host';
    const permissions = Array.isArray(entry.permissions)
      ? entry.permissions.filter((p): p is string => typeof p === 'string' && p in PERMISSION_INFO)
      : [];
    const categories = Array.isArray(entry.categories)
      ? entry.categories.filter((c): c is string => typeof c === 'string' && c in CATEGORY_LABELS)
      : [];
    const platforms = Array.isArray(entry.platforms)
      ? entry.platforms.filter((p): p is string => typeof p === 'string' && p in PLATFORM_LABELS)
      : [];

    const pkg: PluginPackage = {
      target: typeof entry.target === 'string' ? entry.target : '',
      platforms,
      archive: entry.archive,
      sha256: entry.sha256.toLowerCase(),
      bytes: entry.bytes,
    };

    const existing = grouped.get(entry.name);
    if (existing) {
      for (const p of platforms) {
        if (!existing.platforms.includes(p)) existing.platforms.push(p);
      }
      if (!platforms.length) existing.platforms = [];
      if (!existing.packages.some(item => item.archive === pkg.archive && item.target === pkg.target)) {
        existing.packages.push(pkg);
      }
      existing.minBytes = Math.min(existing.minBytes, pkg.bytes);
      continue;
    }

    const mcpServers = Array.isArray(entry.contributes?.mcpServers) ? entry.contributes!.mcpServers! : [];
    const views = Array.isArray(entry.contributes?.views) ? entry.contributes!.views! : [];
    const hasMcp = entry.contributes ? mcpServers.length > 0 : true;
    const hasView = views.length > 0;
    const description = typeof entry.description === 'string' ? entry.description.trim() : '';
    const zh = localizedZhText(entry.i18n);

    grouped.set(entry.name, {
      name: entry.name,
      title: zh?.displayName || (typeof entry.displayName === 'string' && entry.displayName.trim()) || fallbackTitle(entry.name),
      version: entry.version,
      description,
      summary: zh?.description || KNOWN_ZH_SUMMARIES[entry.name] || description,
      kind,
      categories,
      permissions,
      platforms: [...platforms],
      hasMcp,
      hasView,
      mcpServers,
      views,
      runtime: typeof entry.runtime === 'string' ? entry.runtime : '',
      cageEngine: typeof entry.engines?.cage === 'string' ? entry.engines.cage : '>=0.1.8',
      icon: sanitizeIcon(entry.name, entry.icon, baseUrl),
      author: sanitizeAuthor(entry.author),
      source: sanitizeSource(entry.source),
      packages: [pkg],
      minBytes: pkg.bytes,
      repoUrl: `${MARKET_REPO_URL}/tree/main/plugins/${encodeURIComponent(entry.name)}`,
    });
  }

  return [...grouped.values()].sort((a, b) => {
    const aView = a.hasView ? 1 : 0;
    const bView = b.hasView ? 1 : 0;
    const aExample = a.categories.includes('example') && !a.hasView ? 1 : 0;
    const bExample = b.categories.includes('example') && !b.hasView ? 1 : 0;
    if (aExample !== bExample) return aExample - bExample;
    if (a.kind !== b.kind) return a.kind === 'host' ? -1 : 1;
    if (aView !== bView) return bView - aView;
    return a.title.localeCompare(b.title, 'en');
  });
}

export function countByCategory(plugins: MarketPlugin[]): Record<string, number> {
  const counts: Record<string, number> = { all: plugins.length };
  for (const key of CATEGORY_ORDER) counts[key] = 0;
  for (const plugin of plugins) {
    for (const cat of plugin.categories) {
      if (cat in counts) counts[cat]++;
    }
  }
  return counts;
}
