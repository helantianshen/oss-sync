import { DIAGNOSTIC_FOLDER } from "./diagnostic-report";

// 不应进入同步队列的本地文件
//
// 基线、版本控制目录、系统临时文件和 Obsidian 设备本地状态始终忽略
const ABSOLUTE_BLACKLIST: BlacklistEntry[] = [
  { kind: "exact", value: DIAGNOSTIC_FOLDER },
  { kind: "prefix", value: DIAGNOSTIC_FOLDER + "/" },
  { kind: "exact", value: ".oss-sync-state.json" },
  { kind: "exact", value: ".DS_Store" },
  { kind: "exact", value: "Thumbs.db" },
  { kind: "prefix", value: ".git/" },
  { kind: "prefix", value: ".trash/" },
  // 协作目录只通过协作 API 同步，绝不进入普通同步路径
  { kind: "prefix", value: "协作oss/" },
];

const DEVICE_LOCAL_OBSIDIAN: BlacklistEntry[] = [
  { kind: "exact", value: "workspace.json" },
  { kind: "exact", value: "workspace-mobile.json" },
  { kind: "exact", value: "cache" },
  { kind: "prefix", value: "cache/" },
  { kind: "exact", value: "plugins/oss-sync" },
  { kind: "prefix", value: "plugins/oss-sync/" },
  { kind: "glob", value: "plugins/*/data.json" },
];

const PORTABLE_CONFIGURATION = new Set([
  "app.json", "appearance.json", "bookmarks.json", "graph.json", "hotkeys.json",
  "core-plugins.json", "community-plugins.json", "types.json",
]);

export interface SyncPathRules {
  readonly syncConfiguration: boolean;
  readonly syncCommunityPlugins?: boolean;
  readonly ownPluginDir?: string;
  readonly syncExclusions: string;
  readonly syncWhitelist: string;
}

export function configurationRelativePath(path: string, configDir = ""): string | null {
  const root = normalize(configDir).replace(/\/+$/, "");
  const candidate = normalize(path);
  return root && candidate.toLowerCase().startsWith(root.toLowerCase() + "/") ? candidate.slice(root.length + 1) : null;
}

export function isPortableConfiguration(path: string): boolean {
  return PORTABLE_CONFIGURATION.has(path) ||
    /^snippets\/[^/]+\.css$/i.test(path) ||
    /^themes\/[^/]+\/(?:manifest\.json|theme\.css)$/i.test(path);
}

function isCommunityPluginFile(path: string): boolean {
  return /^plugins\/[a-z0-9_-]+\/(?:manifest\.json|main\.js|styles\.css)$/i.test(path);
}

/** 配置开关先约束可移植文件，白名单随后只覆盖用户自己的排除项。 */
export function buildSyncPathFilter(configDir: string, rules: SyncPathRules): (path: string) => boolean {
  const excluded = compilePathRules(rules.syncExclusions);
  const whitelisted = compilePathRules(rules.syncWhitelist);
  return (path) => {
    const normalized = normalize(path);
    if (!shouldSync(normalized, configDir) || normalized.split("/").some((part) => !part || part === "." || part === "..")) return false;
    const ownPluginDir = rules.ownPluginDir?.toLowerCase();
    const lower = normalized.toLowerCase();
    if (ownPluginDir && (lower === ownPluginDir || lower.startsWith(ownPluginDir + "/"))) return false;
    const config = configurationRelativePath(normalized, configDir);
    if (config !== null) {
      if (!rules.syncConfiguration || !(isPortableConfiguration(config) ||
          rules.syncCommunityPlugins && isCommunityPluginFile(config))) return false;
    }
    return whitelisted.some((pattern) => pattern.test(normalized)) ||
      !excluded.some((pattern) => pattern.test(normalized));
  };
}

function compilePathRules(value: string): RegExp[] {
  return (typeof value === "string" ? value : "").split(/\r?\n/).map((line) => normalize(line.trim()).replace(/\/+$/, ""))
    .filter(Boolean).map((pattern) => {
      const tokens = pattern.split(/(\*\*\/|\*\*|\*)/);
      const regex = tokens.map((token) => {
        switch (token) {
          case "**/": return "(?:.*/)?";
          case "**": return ".*";
          case "*": return "[^/]*";
          default: return token.replace(/[.+?^${}()|[\]\\]/g, "\\$&");
        }
      }).join("");
      return new RegExp(`^${regex}(?:/.*)?$`);
    });
}

/** 黑名单条目：exact 为整路径，prefix 为目录前缀，glob 仅支持段内 * */
interface BlacklistEntry {
  kind: "exact" | "prefix" | "glob";
  value: string;
}

/** 判断相对路径是否应同步，设备本地状态始终排除 */
export function shouldSync(relativePath: string, configDir: string): boolean {
  const p = normalize(relativePath);
  if (p === "") return false;
  if ([DIAGNOSTIC_FOLDER, ".git", ".trash", "协作oss"].includes(p)) return false;

  for (const entry of ABSOLUTE_BLACKLIST) {
    if (matches(entry, p)) return false;
  }
  const prefix = normalize(configDir).replace(/\/$/, "") + "/";
  if (p.toLowerCase().startsWith(prefix.toLowerCase())) {
    const configPath = p.slice(prefix.length).toLowerCase();
    for (const entry of DEVICE_LOCAL_OBSIDIAN) {
      if (matches(entry, configPath)) return false;
    }
  }
  return true;
}

function normalize(p: string): string {
  return p.replace(/\\/g, "/").replace(/^\.\/+/, "");
}

function matches(entry: BlacklistEntry, path: string): boolean {
  switch (entry.kind) {
    case "exact":
      return path === entry.value;
    case "prefix":
      return path === entry.value || path.startsWith(entry.value);
    case "glob":
      return globMatch(entry.value, path);
  }
}

// 这里只需要支持路径段内的 *
function globMatch(pattern: string, path: string): boolean {
  const regexStr = pattern
    .split("/")
    .map((seg) => seg.replace(/[.+?^${}()|[\]\\]/g, "\\$&").replace(/\*/g, "[^/]*"))
    .join("/");
  return new RegExp("^" + regexStr + "$").test(path);
}

/** 仅供测试访问黑名单表与匹配逻辑 */
export const _internal = {
  ABSOLUTE_BLACKLIST,
  DEVICE_LOCAL_OBSIDIAN,
  matches,
  normalize,
};

/** 按扩展名和当前 Vault 配置目录判断同步文件类型 */
export function classifySyncPath(path: string, configDir: string): "markdown" | "attachment" | "config" {
  const normalized = normalize(path).toLowerCase();
  if (normalized.endsWith(".md")) return "markdown";
  const prefix = normalize(configDir).replace(/\/$/, "").toLowerCase() + "/";
  return normalized.startsWith(prefix) ? "config" : "attachment";
}
