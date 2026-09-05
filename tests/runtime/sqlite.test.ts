// MARK: SQLite contract
//
// Exercises the real persistence path (`createDbClient`, the same entry
// point the production server uses) against a temporary database: schema
// migrations apply, reads/writes round-trip, failed transactions roll
// back, concurrent writers do not corrupt the database, and data
// survives a client restart.
//
// The migration runner resolves "./drizzle" relative to the process
// working directory, so this suite must run with the repository root as
// CWD (true for `pnpm run test:runtime` and CI).

import { existsSync } from "node:fs";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { eq } from "drizzle-orm";
import { afterAll, beforeAll, describe, expect, test } from "vitest";

import { createDbClient } from "~/server/db/client.server";
import { users } from "~/server/db/schema";

type DbClient = Awaited<ReturnType<typeof createDbClient>>;

describe("SQLite contract", () => {
  let dir: string;
  let dbPath: string;
  let db: DbClient;

  beforeAll(async () => {
    // Fail fast with a clear message if the CWD assumption is violated.
    expect(
      existsSync(resolve("drizzle")),
      "expected ./drizzle migrations relative to CWD",
    ).toBe(true);

    dir = await mkdtemp(join(tmpdir(), "headplane-sqlite-test-"));
    dbPath = join(dir, "contract.db");
    db = await createDbClient(dbPath);
  });

  afterAll(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  test("migrations apply and the database file is created", async () => {
    expect(existsSync(dbPath)).toBe(true);
    const rows = await db.select().from(users);
    expect(rows).toEqual([]);
  });

  test("inserts round-trip with typed columns", async () => {
    await db.insert(users).values({ id: "u-roundtrip", sub: "sub-1", name: "Ada" });
    const rows = await db.select().from(users).where(eq(users.id, "u-roundtrip"));
    expect(rows).toHaveLength(1);
    expect(rows[0].sub).toBe("sub-1");
    expect(rows[0].name).toBe("Ada");
    expect(rows[0].role).toBe("member");
    expect(rows[0].created_at).toBeInstanceOf(Date);
  });

  test("a failed transaction rolls back", async () => {
    await expect(
      db.transaction(async (tx) => {
        await tx.insert(users).values({ id: "u-rollback", sub: "sub-rollback" });
        throw new Error("boom");
      }),
    ).rejects.toThrow("boom");

    const rows = await db.select().from(users).where(eq(users.id, "u-rollback"));
    expect(rows).toEqual([]);
  });

  test("concurrent writers do not lose or corrupt rows", async () => {
    const count = 20;
    await Promise.all(
      Array.from({ length: count }, (_, i) =>
        db.insert(users).values({ id: `u-concurrent-${i}`, sub: `sub-concurrent-${i}` }),
      ),
    );
    const rows = await db.select({ id: users.id }).from(users);
    const ids = new Set(rows.map((r) => r.id));
    for (let i = 0; i < count; i++) {
      expect(ids.has(`u-concurrent-${i}`)).toBe(true);
    }
  });

  test("data survives a client restart", async () => {
    await db.insert(users).values({ id: "u-restart", sub: "sub-restart" });

    const reopened = await createDbClient(dbPath);
    const rows = await reopened.select().from(users).where(eq(users.id, "u-restart"));
    expect(rows).toHaveLength(1);
    expect(rows[0].sub).toBe("sub-restart");
  });
});
