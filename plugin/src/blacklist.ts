// 不应进入同步队列的本地文件。
//
// 基线、版本控制目录和系统临时文件始终忽略；.obsidian 下的本地状态
// 默认忽略，但用户可以在设置中开启。
const ABSOLUTE_BLACKLIST: BlacklistEntry[] = [
  { kind: "exact", value: ".oss-sync-state.json" },
  { kind: "exact", value: ".DS_Store" },
  { kind: "exact", value: "Thumbs.db" },
  { kind: "prefix", value: ".git/" },
  { kind: "prefix", value: ".trash/" },
];

const DEVICE_LOCAL_OBSIDIAN: BlacklistEntry[] = [
  { kind: "exact", value: ".obsidian/workspace.json" },
  { kind: "exact", value: ".obsidian/workspace-mobile.json" },
  { kind: "prefix", value: ".obsidian/cache/" },
  { kind: "glob", value: ".obsidian/plugins/*/data.json" },
];

interface BlacklistEntry {
  kind: "exact" | "prefix" | "glob";
  value: string;
}

/**
 * 判断 Vault 相对路径是否应进入同步队列。
 *
 * @param relativePath vault 内相对路径（Posix 风格 / 分隔）
 * @param allowDeviceLocalObsidian 是否允许同步设备相关的 Obsidian 状态文件
 */
export function shouldSync(relativePath: string, allowDeviceLocalObsidian: boolean): boolean {
  const p = normalize(relativePath);
  if (p === "") return false;

  for (const entry of ABSOLUTE_BLACKLIST) {
    if (matches(entry, p)) return false;
  }
  if (!allowDeviceLocalObsidian) {
    for (const entry of DEVICE_LOCAL_OBSIDIAN) {
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

// 这里只需要支持路径段内的 *。
function globMatch(pattern: string, path: string): boolean {
  const regexStr = pattern
    .split("/")
    .map((seg) => seg.replace(/\*/g, "[^/]*"))
    .join("/");
  return new RegExp("^" + regexStr + "$").test(path);
}

export const _internal = {
  ABSOLUTE_BLACKLIST,
  DEVICE_LOCAL_OBSIDIAN,
  matches,
  normalize,
};
