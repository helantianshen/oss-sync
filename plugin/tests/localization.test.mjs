import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import { pathToFileURL } from "node:url";
import { build } from "esbuild";

async function loadI18n() {
  const dir = await mkdtemp(join(tmpdir(), "oss-i18n-"));
  const outfile = join(dir, "i18n.mjs");
  await build({
    entryPoints: ["src/i18n.ts"],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
  });
  const module = await import(pathToFileURL(outfile).href);
  return { ...module, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

async function loadLocalizedError() {
  const dir = await mkdtemp(join(tmpdir(), "oss-localized-error-"));
  const outfile = join(dir, "localized-error.mjs");
  await build({
    entryPoints: ["src/localized-error.ts"],
    outfile,
    bundle: true,
    platform: "node",
    format: "esm",
  });
  const module = await import(pathToFileURL(outfile).href);
  return { ...module, cleanup: () => rm(dir, { recursive: true, force: true }) };
}

test("auto language follows Chinese Obsidian locales and otherwise uses English", async () => {
  const { resolveLanguage, cleanup } = await loadI18n();
  try {
    assert.equal(resolveLanguage("auto", "zh-CN"), "zh");
    assert.equal(resolveLanguage("auto", "zh-TW"), "zh");
    assert.equal(resolveLanguage("auto", "en-US"), "en");
    assert.equal(resolveLanguage("auto", "ja"), "en");
    assert.equal(resolveLanguage("zh", "en-US"), "zh");
    assert.equal(resolveLanguage("en", "zh-CN"), "en");
  } finally {
    await cleanup();
  }
});

test("Chinese and English dictionaries expose the same complete key set", async () => {
  const { TRANSLATIONS, cleanup } = await loadI18n();
  try {
    assert.deepEqual(Object.keys(TRANSLATIONS.zh).sort(), Object.keys(TRANSLATIONS.en).sort());
    assert.ok(Object.keys(TRANSLATIONS.en).length >= 80);
  } finally {
    await cleanup();
  }
});

test("dictionaries expose conflict-section keys and drop sidebar activity keys", async () => {
  const { TRANSLATIONS, cleanup } = await loadI18n();
  try {
    for (const lang of ["zh", "en"]) {
      for (const key of ["sidebar.conflicts", "sidebar.resolveConflict"]) {
        assert.ok(key in TRANSLATIONS[lang], `${lang} is missing ${key}`);
      }
      for (const key of ["sidebar.activity", "sidebar.noActivity"]) {
        assert.ok(!(key in TRANSLATIONS[lang]), `${lang} must not keep ${key}`);
      }
    }
  } finally {
    await cleanup();
  }
});

test("English mode contains no Chinese UI fragments and formats parameters", async () => {
  const { TRANSLATIONS, translate, cleanup } = await loadI18n();
  try {
    for (const [key, value] of Object.entries(TRANSLATIONS.en)) {
      assert.doesNotMatch(value, /[\u3400-\u9fff]/u, key);
    }
    assert.equal(
      translate("en", "notice.syncFailures", { count: 3 }),
      "OSS: 3 sync tasks failed and will retry next time"
    );
    assert.equal(translate("zh", "sidebar.user", { value: "小明" }), "用户：小明");
  } finally {
    await cleanup();
  }
});

test("network connection refusal is localized without replacing server error details", async () => {
  const { localizeError, cleanup } = await loadLocalizedError();
  try {
    const zh = (key) => ({ "network.connectionRefused": "无法连接到服务器" })[key] ?? key;
    const en = (key) => ({ "network.connectionRefused": "Unable to connect to the server" })[key] ?? key;
    assert.equal(localizeError(new Error("net::ERR_CONNECTION_REFUSED"), zh, "未知错误"), "无法连接到服务器");
    assert.equal(localizeError(new Error("net::ERR_CONNECTION_REFUSED"), en, "Unknown error"), "Unable to connect to the server");
    assert.equal(localizeError(new Error("服务器返回配额不足"), zh, "未知错误"), "服务器返回配额不足");
    assert.equal(
      localizeError({ code: "project_storage_quota_exceeded" }, (key) => key === "storage.projectQuotaExceeded" ? "项目空间已满" : key, "未知错误"),
      "项目空间已满"
    );
  } finally {
    await cleanup();
  }
});

test("redirect loops from unauthenticated requests are reported as connection failures", async () => {
  const { localizeError, cleanup } = await loadLocalizedError();
  try {
    const t = (key) => ({ "auth.unauthorized": "连接失败" })[key] ?? key;
    assert.equal(localizeError(new Error("net::ERR_TOO_MANY_REDIRECTS"), t, "未知错误"), "连接失败");
  } finally {
    await cleanup();
  }
});

test("401 and 403 are not misreported as wrong credentials", async () => {
  const { localizeError, cleanup } = await loadLocalizedError();
  try {
    const t = (key) => ({
      "auth.unauthorized": "连接失败",
      "auth.forbidden": "连接失败",
      "auth.invalidCredentials": "用户名或密码错误",
    })[key] ?? key;
    class ApiError extends Error {
      constructor(message, status) {
        super(message);
        this.status = status;
      }
    }

    assert.equal(localizeError(new ApiError("unauthorized: invalid credentials", 401), t, "未知错误"), "连接失败");
    assert.equal(localizeError(new ApiError("invalid jwt token", 401), t, "未知错误"), "连接失败");
    assert.equal(localizeError(new ApiError("forbidden", 403), t, "未知错误"), "连接失败");
  } finally {
    await cleanup();
  }
});

test("token expiry on 401 still resolves to the login prompt", async () => {
  const { localizeError, cleanup } = await loadLocalizedError();
  try {
    const t = (key) => ({
      "auth.tokenExpired": "登录已过期",
      "auth.unauthorized": "连接失败",
    })[key] ?? key;
    class ApiError extends Error {
      constructor(message) {
        super(message);
        this.status = 401;
      }
    }

    assert.equal(localizeError(new ApiError("jwt token expired"), t, "未知错误"), "登录已过期");
  } finally {
    await cleanup();
  }
});

test("unrelated server prose is still surfaced verbatim", async () => {
  const { localizeError, cleanup } = await loadLocalizedError();
  try {
    const t = (key) => key;
    assert.equal(localizeError(new Error("vault quota exceeded"), t, "未知错误"), "vault quota exceeded");
  } finally {
    await cleanup();
  }
});
