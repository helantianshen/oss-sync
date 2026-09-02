import assert from "node:assert/strict";
import test from "node:test";
import { loadApiClient } from "./helpers/api-loader.mjs";

function settings(serverUrl) {
  return {
    serverUrl,
    username: "",
    password: "",
    syncIntervalSec: 3,
    maxConcurrency: 6,
    syncPoisonObsidianFiles: false,
    incrementalCheck: true,
    keepDirectoryTree: true,
    vaultId: "vault-1",
    vaultName: "Vault",
    clientId: "client 1",
    deviceName: "Device",
    remotePollIntervalSec: 30,
    vaultSyncMode: "short_poll",
    language: "en",
  };
}

test("builds a collaboration SSE URL only for HTTPS or loopback HTTP", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = undefined;
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    const cases = [
      ["https://sync.example.com", true],
      ["http://localhost:9090", true],
      ["http://127.0.0.1:9090", true],
      ["http://[::1]:9090", true],
      ["http://sync.example.com:9090", false],
      ["http://192.168.1.5:9090", false],
      ["not a url", false],
    ];

    for (const [serverUrl, expected] of cases) {
      const client = new OSSApiClient(settings(serverUrl));
      client.setToken("token value");
      const streamUrl = client.collabEventStreamURL("vault 1");
      assert.equal(streamUrl !== null, expected, serverUrl);
      if (expected) {
        const parsed = new URL(streamUrl);
        assert.equal(parsed.pathname, "/api/vaults/vault%201/collaborations/stream");
        assert.equal(parsed.searchParams.get("token"), "token value");
        assert.equal(parsed.searchParams.get("client_id"), "client 1");
      }
    }
  } finally {
    await cleanup();
  }
});

test("does not expose the collaboration token without an authenticated session", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = undefined;
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    const client = new OSSApiClient(settings("http://localhost:9090"));
    assert.equal(client.collabEventStreamURL("vault-1"), null);
  } finally {
    await cleanup();
  }
});

test("builds the account-wide collaboration SSE URL for a safe server", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = undefined;
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    const client = new OSSApiClient(settings("http://localhost:9090"));
    client.setToken("token value");

    const streamUrl = client.collabAccountEventStreamURL();

    assert.notEqual(streamUrl, null);
    const parsed = new URL(streamUrl);
    assert.equal(parsed.pathname, "/api/collaborations/stream");
    assert.equal(parsed.searchParams.get("token"), "token value");
  } finally {
    await cleanup();
  }
});

test("records safe status, duration, and byte metadata for binary transfers", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = {
    status: 200,
    json: { path: "Notes/Private.md", type: "markdown", hash: "hash", size: 4, mtime: 1, revision: 2, deleted: false },
    text: "",
    arrayBuffer: new Uint8Array([1, 2, 3, 4]).buffer,
    headers: {},
  };
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    const events = [];
    const diagnostics = { record: (event) => events.push(event) };
    const client = new OSSApiClient(settings("http://localhost:9090"), diagnostics);
    client.setToken("s3cr3t-token");

    // Given: binary upload and download transport responses.
    // When: both transfers complete.
    await client.uploadV2("vault-1", {
      path: "Notes/Private.md",
      baseRevision: 1,
      hash: "hash",
      mtime: 1,
      operationID: "operation",
      content: new Uint8Array([1, 2, 3, 4]).buffer,
    });
    await client.downloadV2("vault-1", "Notes/Private.md", 2);

    // Then: diagnostics retain status and byte counts but omit secrets, paths, and bodies.
    assert.deepEqual(events.filter((event) => event.kind === "api").map((event) => event.status), [200, 200]);
    assert.deepEqual(events.filter((event) => event.kind === "transfer").map((event) => event.bytes), [4, 4]);
    const serialized = JSON.stringify(events);
    for (const forbidden of ["s3cr3t-token", "Notes/Private.md", "Authorization", "content"]) {
      assert.equal(serialized.includes(forbidden), false, forbidden);
    }
  } finally {
    await cleanup();
  }
});

test("clears authentication and runs recovery when the device token expires", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = {
    status: 401,
    json: { error: "unauthorized", code: "token_expired" },
    text: "",
    arrayBuffer: new ArrayBuffer(0),
    headers: {},
  };
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    let recoveries = 0;
    const client = new OSSApiClient(
      settings("http://localhost:9090"),
      undefined,
      async () => { recoveries += 1; },
    );
    client.setToken("expired-token");

    await assert.rejects(
      client.uploadV2("vault-1", {
        path: "expired.md",
        baseRevision: 1,
        hash: "hash",
        mtime: 1,
        operationID: "operation",
        content: new ArrayBuffer(0),
      }),
      (error) => error.code === "token_expired",
    );

    assert.equal(client.hasToken(), false);
    assert.equal(recoveries, 1);
  } finally {
    await cleanup();
  }
});

test("retains authentication for non-expiry authorization failures", async () => {
  globalThis.__ossRequests = [];
  globalThis.__ossResponse = {
    status: 403,
    json: { error: "device pending", code: "device_pending" },
    text: "",
    arrayBuffer: new ArrayBuffer(0),
    headers: {},
  };
  const { OSSApiClient, cleanup } = await loadApiClient();
  try {
    let recoveries = 0;
    const client = new OSSApiClient(
      settings("http://localhost:9090"),
      undefined,
      async () => { recoveries += 1; },
    );
    client.setToken("valid-token");

    await assert.rejects(client.authStatus(), (error) => error.code === "device_pending");

    assert.equal(client.hasToken(), true);
    assert.equal(recoveries, 0);
  } finally {
    await cleanup();
  }
});
