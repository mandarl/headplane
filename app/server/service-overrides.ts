import { eq } from "drizzle-orm";
import { NodeSQLiteDatabase } from "drizzle-orm/node-sqlite";

import { serviceDescriptionOverrides } from "./db/schema";

export interface ServiceOverride {
  description: string;
  updatedBy: string | null;
  // ISO string (not a `Date`) so this can flow straight into loader data,
  // matching how the rest of this route serializes timestamps (see
  // `agentSync.syncedAt?.toISOString()` in machine.tsx's loader).
  updatedAt: string | null;
}

/**
 * Fetches all Headplane-side service description overrides for a node,
 * keyed by `${proto}:${port}` so they're easy to look up against entries
 * from `HostInfo.Services` at render time.
 */
export async function getServiceOverrides(
  db: NodeSQLiteDatabase,
  hostId: string,
): Promise<Record<string, ServiceOverride>> {
  const rows = await db
    .select()
    .from(serviceDescriptionOverrides)
    .where(eq(serviceDescriptionOverrides.host_id, hostId));

  const overrides: Record<string, ServiceOverride> = {};
  for (const row of rows) {
    overrides[`${row.proto}:${row.port}`] = {
      description: row.description,
      updatedBy: row.updated_by,
      updatedAt: row.updated_at?.toISOString() ?? null,
    };
  }

  return overrides;
}

/**
 * Sets (or replaces) the description override for a single service. An
 * empty/whitespace-only description clears the override instead, reverting
 * the UI back to Tailscale's auto-detected value on next render.
 */
export async function setServiceOverride(
  db: NodeSQLiteDatabase,
  params: {
    hostId: string;
    proto: string;
    port: number;
    description: string;
    updatedBy: string | null;
  },
): Promise<void> {
  const { hostId, proto, port, updatedBy } = params;
  const description = params.description.trim();
  const id = `${hostId}:${proto}:${port}`;

  if (description === "") {
    await db.delete(serviceDescriptionOverrides).where(eq(serviceDescriptionOverrides.id, id));
    return;
  }

  await db
    .insert(serviceDescriptionOverrides)
    .values({
      id,
      host_id: hostId,
      proto,
      port,
      description,
      updated_by: updatedBy,
      updated_at: new Date(),
    })
    .onConflictDoUpdate({
      target: serviceDescriptionOverrides.id,
      set: {
        description,
        updated_by: updatedBy,
        updated_at: new Date(),
      },
    });
}
