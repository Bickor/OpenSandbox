// Copyright 2026 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

import { createClient } from "redis";
import { expect, test } from "vitest";

import {
  AcquirePolicy,
  PoolDestroyedException,
  PoolLifecycleState,
  Sandbox,
  SandboxPool,
  SandboxPoolManager,
} from "@alibaba-group/opensandbox";
import { RedisPoolStateStore } from "@alibaba-group/opensandbox/pool-redis";

import { createConnectionConfig, getSandboxImage } from "./base_e2e.ts";

const redisUrl = process.env.OPENSANDBOX_TEST_REDIS_URL;
const redisTest = redisUrl ? test : test.skip;

async function eventually(check: () => Promise<boolean>, timeoutMs = 120_000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await check()) return;
    await new Promise((resolve) => setTimeout(resolve, 500));
  }
  throw new Error("distributed pool did not converge before timeout");
}

redisTest("Redis pool coordinates acquire, resize, failover fencing, and destroy", async () => {
  const poolName = `js-pool-redis-${Math.random().toString(16).slice(2, 10)}`;
  const connectionConfig = createConnectionConfig();
  const redisA = createClient({ url: redisUrl });
  const redisB = createClient({ url: redisUrl });
  await Promise.all([redisA.connect(), redisB.connect()]);
  const storeA = new RedisPoolStateStore({ client: redisA });
  const storeB = new RedisPoolStateStore({ client: redisB });
  const common = {
    poolName,
    maxIdle: 1,
    connectionConfig,
    creationSpec: {
      image: getSandboxImage(),
      metadata: { tag: poolName },
      resource: { cpu: "1", memory: "2Gi" },
    },
    idleTimeoutSeconds: 5 * 60,
    warmupReadyTimeoutSeconds: 60,
  };
  const poolA = SandboxPool.create({ ...common, ownerId: `${poolName}-a`, stateStore: storeA });
  const poolB = SandboxPool.create({ ...common, ownerId: `${poolName}-b`, stateStore: storeB });
  const manager = new SandboxPoolManager({ stateStore: storeA, connectionConfig });
  let acquired: Sandbox | undefined;

  try {
    await Promise.all([poolA.start(), poolB.start()]);
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);

    acquired = await poolB.acquire({
      policy: AcquirePolicy.FAIL_FAST,
      sandboxTimeoutSeconds: 5 * 60,
    });
    expect(await acquired.isHealthy()).toBe(true);
    await eventually(async () => (await poolA.snapshot()).idleCount === 1);

    await poolB.resize(2);
    await eventually(async () => (await poolA.snapshot()).idleCount === 2);

    const result = await manager.destroy(poolName, { tombstoneTtlSeconds: 60 });
    expect(result.drainedIdleCount).toBe(2);
    expect(result.killedIdleCount).toBe(2);
    await expect(poolA.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await expect(poolB.acquire()).rejects.toBeInstanceOf(PoolDestroyedException);
    await eventually(async () =>
      (await poolA.snapshot()).lifecycleState === PoolLifecycleState.STOPPED &&
      (await poolB.snapshot()).lifecycleState === PoolLifecycleState.STOPPED,
    );
  } finally {
    await poolA.shutdown(false).catch(() => undefined);
    await poolB.shutdown(false).catch(() => undefined);
    await acquired?.kill().catch(() => undefined);
    await acquired?.close().catch(() => undefined);
    await Promise.all([
      redisA.close().catch(() => undefined),
      redisB.close().catch(() => undefined),
    ]);
  }
}, 10 * 60_000);
