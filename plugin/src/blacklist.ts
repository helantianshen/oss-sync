// 同步黑名单 — 决策 6.1（绝对黑名单，硬编码）+ 决策 6.2（.obsidian 毒药文件过滤）。
//
// 规则：
//   - 决策 6.1：下列文件/目录在任何情况下都不同步，用户不可配置关闭。
//     原因：把它们同步上去会污染基线，导致冲突检测逻辑彻底崩溃。
//   - 决策 6.2：.obsidian/ 下的 workspace*.json、cache、插件 data.json 等
//     跨端意义不大且易冲突；默认过滤，用户可在设置中开启（默认关）。
//
// 调用点：vault 'modify'/'create'/'delete' 三监听器内需先过 shouldSync()。

/** 决策 6.1：绝对黑名单（用户不可关闭）。 */
const ABSOLUTE_BLACKLIST: BlacklistEntry[] = [
  // 项目自身基线
  { kind: "exact", value: ".oss-sync-state.json" },
  // 系统/版本控制噪声
  { kind: "exact", value: ".DS_Store" },
  { kind: "exact", value: "Thumbs.db" },
  { kind: "prefix", value: ".git/" },
  // Obsidian 自带回收站
  { kind: "prefix", value: ".trash/" },
];

/** 决策 6.2：.obsidian/ 毒药文件（默认关，用户可开）。 */
const POISON_OBSIDIAN: BlacklistEntry[] = [
  { kind: "exact", value: ".obsidian/workspace.json" },
  { kind: "exact", value: ".obsidian/workspace-mobile.json" },
  { kind: "prefix", value: ".obsidian/cache/" },
  // 插件本地状态
  { kind: "glob", value: ".obsidian/plugins/*/data.json" },
];

interface BlacklistEntry {
  kind: "exact" | "prefix" | "glob";
  value: string;
}

/**
 * shouldSync 决定一个 vault 相对路径是否应该被纳入同步。
 * 返回 true 表示可以通过；false 表示必须跳过。
 *
 * @param relativePath vault 内相对路径（Posix 风格 / 分隔）
 * @param allowPoisonObsidian 决策 6.2 开关
 */
export function shouldSync(relativePath: string, allowPoisonObsidian: boolean): boolean {
  const p = normalize(relativePath);
  if (p === "") return false;

  for (const entry of ABSOLUTE_BLACKLIST) {
    if (matches(entry, p)) return false;
  }
  if (!allowPoisonObsidian) {
    for (const entry of POISON_OBSIDIAN) {
      if (matches(entry, p)) return false;
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

// 极简 glob：仅支持 * 在段内匹配任意非 / 字符。
function globMatch(pattern: string, path: string): boolean {
  const regexStr = pattern
    .split("/")
    .map((seg) => seg.replace(/\*/g, "[^/]*"))
    .join("/");
  return new RegExp("^" + regexStr + "$").test(path);
}

/** 测试辅助：导出内部规则列表，便于单元测试断言。 */
export const _internal = {
  ABSOLUTE_BLACKLIST,
  POISON_OBSIDIAN,
  matches,
  normalize,
};
