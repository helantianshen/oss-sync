// 插件文件原子替换与无感重载
//
// 流程：备份现有三件套 → 先写临时文件再 rename 覆盖目标（每步失败都回滚）→
// disablePlugin/enablePlugin 重载 → 校验新版本实例已加载，失败则恢复备份并重新启用旧版本

import {
  manifestVersionFromText,
  type UpdateFile,
} from "./plugin-update";

export interface PluginFileAdapter {
  read(path: string): Promise<string | null>;
  write(path: string, data: string | ArrayBuffer): Promise<void>;
  rename(oldPath: string, newPath: string): Promise<void>;
  remove(path: string): Promise<void>;
}

export interface ReloadController {
  disablePlugin(id: string): Promise<void>;
  enablePlugin(id: string): Promise<void>;
  /** 插件实例是否已按目标版本加载*/
  isLoaded(id: string, expectedVersion?: string | null): boolean;
}

export interface ApplyUpdateOptions {
  adapter: PluginFileAdapter;
  reload: ReloadController;
  dir: string;
  pluginID: string;
  files: readonly UpdateFile[];
}

const TEMP_SUFFIX = ".oss-update-tmp";

function joinPath(dir: string, name: string): string {
  return dir.replace(/\/+$/, "") + "/" + name;
}

function textOf(content: ArrayBuffer): string {
  return new TextDecoder().decode(content);
}

/** 原子替换插件文件并重载插件；任何一步失败都会恢复旧文件并重新启用旧版本*/
export async function applyPluginUpdate(options: ApplyUpdateOptions): Promise<void> {
  const backup = await readBackup(options.adapter, options.dir, options.files);
  await replacePluginFiles(options.adapter, options.dir, options.files, backup);

  const manifestFile = options.files.find((file) => file.name === "manifest.json");
  const expectedVersion = manifestFile
    ? manifestVersionFromText(textOf(manifestFile.content))
    : null;

  try {
    await options.reload.disablePlugin(options.pluginID);
  } catch {
    // 插件可能已禁用或未加载，继续尝试启用
  }

  try {
    await options.reload.enablePlugin(options.pluginID);
  } catch (error) {
    await rollbackAndReload(options, backup);
    throw error;
  }

  if (!options.reload.isLoaded(options.pluginID, expectedVersion)) {
    await rollbackAndReload(options, backup);
    throw new Error("plugin reload failed after update; previous version restored");
  }
}

async function rollbackAndReload(
  options: ApplyUpdateOptions,
  backup: ReadonlyMap<string, string | null>
): Promise<void> {
  await restoreFiles(options.adapter, options.dir, options.files, backup);
  try {
    await options.reload.disablePlugin(options.pluginID);
  } catch {
    // 忽略重载失败，原插件保持禁用状态即可
  }
  try {
    await options.reload.enablePlugin(options.pluginID);
  } catch {
    // 旧版本也可能加载失败，不掩盖原始错误
  }
}

async function readBackup(
  adapter: PluginFileAdapter,
  dir: string,
  files: readonly UpdateFile[]
): Promise<Map<string, string | null>> {
  const backup = new Map<string, string | null>();
  for (const file of files) {
    try {
      backup.set(file.name, await adapter.read(joinPath(dir, file.name)));
    } catch {
      backup.set(file.name, null);
    }
  }
  return backup;
}

/** 先写临时文件，再逐个 rename 覆盖目标；任一步失败都恢复已覆盖的目标并清理临时文件*/
async function replacePluginFiles(
  adapter: PluginFileAdapter,
  dir: string,
  files: readonly UpdateFile[],
  backup: ReadonlyMap<string, string | null>
): Promise<void> {
  const temps = files.map((file) => `${file.name}${TEMP_SUFFIX}`);

  try {
    for (let i = 0; i < files.length; i++) {
      // Obsidian 的 adapter.write 只接受字符串；插件三件套均为 UTF-8 可解码内容
      await adapter.write(joinPath(dir, temps[i]), textOf(files[i].content));
    }
  } catch (error) {
    await removePaths(adapter, dir, temps);
    throw error;
  }

  for (let i = 0; i < files.length; i++) {
    try {
      await adapter.rename(joinPath(dir, temps[i]), joinPath(dir, files[i].name));
    } catch (error) {
      await restoreTargets(adapter, dir, files, backup, i);
      await removePaths(adapter, dir, temps.slice(i));
      throw error;
    }
  }
  await removePaths(adapter, dir, temps).catch(() => undefined);
}

/** 恢复已覆盖的 files[0..upTo) 目标文件*/
async function restoreTargets(
  adapter: PluginFileAdapter,
  dir: string,
  files: readonly UpdateFile[],
  backup: ReadonlyMap<string, string | null>,
  upTo: number
): Promise<void> {
  for (let i = 0; i < upTo; i++) {
    await restoreOne(adapter, dir, files[i].name, backup.get(files[i].name));
  }
}

async function restoreFiles(
  adapter: PluginFileAdapter,
  dir: string,
  files: readonly UpdateFile[],
  backup: ReadonlyMap<string, string | null>
): Promise<void> {
  await restoreTargets(adapter, dir, files, backup, files.length);
}

async function restoreOne(
  adapter: PluginFileAdapter,
  dir: string,
  name: string,
  content: string | null | undefined
): Promise<void> {
  const path = joinPath(dir, name);
  if (content == null) {
    await adapter.remove(path).catch(() => undefined);
  } else {
    await adapter.write(path, content);
  }
}

async function removePaths(
  adapter: PluginFileAdapter,
  dir: string,
  names: readonly string[]
): Promise<void> {
  await Promise.all(
    names.map((name) => adapter.remove(joinPath(dir, name)).catch(() => undefined))
  );
}
