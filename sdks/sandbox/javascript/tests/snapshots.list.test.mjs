import assert from "node:assert/strict";
import test from "node:test";

import { SandboxesAdapter } from "../dist/internal.js";

test("listSnapshots forwards the exact name filter", async () => {
  let captured;
  const client = {
    async GET(path, options) {
      captured = { path, query: options.params.query };
      return {
        data: {
          items: [],
          pagination: {
            page: 1,
            pageSize: 20,
            totalItems: 0,
            totalPages: 0,
            hasNextPage: false,
          },
        },
        response: new Response(null, { status: 200 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  await adapter.listSnapshots({ name: "toolchain:node@rev-1" });

  assert.deepEqual(captured, {
    path: "/snapshots",
    query: { name: "toolchain:node@rev-1" },
  });
});

test("createSnapshot sends format and exposes response restore metadata", async () => {
  let capturedBody;
  const client = {
    async POST(path, options) {
      assert.equal(path, "/sandboxes/{sandboxId}/snapshots");
      capturedBody = options.body;
      return {
        data: {
          id: "snap-1",
          sandboxId: "sbx-1",
          format: "future-v2",
          restoreConstraints: {
            placement: "same-node",
            sourceNode: "node-1",
            durable: false,
          },
          status: { state: "Ready" },
          createdAt: "2026-09-23T12:00:00Z",
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  const snapshot = await adapter.createSnapshot("sbx-1", {
    name: "baseline",
    format: "kata-vmstate-v1",
  });

  assert.deepEqual(capturedBody, {
    name: "baseline",
    format: "kata-vmstate-v1",
  });
  assert.equal(snapshot.format, "future-v2");
  assert.deepEqual(snapshot.restoreConstraints, {
    placement: "same-node",
    sourceNode: "node-1",
    durable: false,
  });
  assert.ok(snapshot.createdAt instanceof Date);
});

test("createSnapshot omits format when not requested", async () => {
  let capturedBody;
  const client = {
    async POST(_path, options) {
      capturedBody = options.body;
      return {
        data: {
          id: "snap-1",
          sandboxId: "sbx-1",
          status: { state: "Creating" },
          createdAt: "2026-09-23T12:00:00Z",
        },
        response: new Response(null, { status: 202 }),
      };
    },
  };

  const adapter = new SandboxesAdapter(client);
  await adapter.createSnapshot("sbx-1", { name: "baseline" });

  assert.deepEqual(capturedBody, { name: "baseline" });
});
