import { data, redirect } from "react-router";

import {
  authContext,
  dbContext,
  headscaleLiveStoreContext,
  requestApiContext,
} from "~/server/context";
import { isDataWithApiError } from "~/server/headscale/api/error-client";
import { nodesResource } from "~/server/headscale/live-store";
import { setServiceOverride } from "~/server/service-overrides";
import { Capabilities } from "~/server/web/roles";
import { normalizeRegistrationKey } from "~/utils/register-key";

import type { Route } from "./+types/machine";

export async function machineAction({ request, context }: Route.ActionArgs) {
  const auth = context.get(authContext);
  const db = context.get(dbContext);
  const getRequestApi = context.get(requestApiContext);
  const headscaleLiveStore = context.get(headscaleLiveStoreContext);

  const { principal, api } = await getRequestApi(request);

  const formData = await request.formData();

  const action = formData.get("action_id")?.toString();
  if (!action) {
    throw data("Missing `action_id` in the form data.", {
      status: 400,
    });
  }

  // Fast track register since it doesn't require an existing machine
  if (action === "register") {
    if (!auth.can(principal, Capabilities.write_machines)) {
      throw data("You do not have permission to manage machines", {
        status: 403,
      });
    }

    const registrationKeyInput = formData.get("register_key")?.toString();
    if (!registrationKeyInput) {
      throw data("Missing `register_key` in the form data.", {
        status: 400,
      });
    }

    const registrationKey = normalizeRegistrationKey(registrationKeyInput);
    if (!registrationKey) {
      throw data("Invalid `register_key` in the form data.", {
        status: 400,
      });
    }

    const user = formData.get("user")?.toString();
    if (!user) {
      throw data("Missing `user` in the form data.", {
        status: 400,
      });
    }

    const node = await api.nodes.register(user, registrationKey);
    await headscaleLiveStore.refresh(nodesResource, api);
    return redirect(`/machines/${node.id}`);
  }

  // Check if the user has permission to manage this machine
  const nodeId = formData.get("node_id")?.toString();
  if (!nodeId) {
    throw data("Missing `node_id` in the form data.", {
      status: 400,
    });
  }

  const node = await api.nodes.get(nodeId);
  if (!node) {
    throw data(`Machine with ID ${nodeId} not found`, {
      status: 404,
    });
  }

  if (!auth.canManageNode(principal, node)) {
    throw data("You do not have permission to act on this machine", {
      status: 403,
    });
  }

  switch (action) {
    case "rename": {
      const newName = formData.get("name")?.toString();
      if (!newName) {
        throw data("Missing `name` in the form data.", {
          status: 400,
        });
      }

      const name = String(formData.get("name"));
      if (!/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(name.toLowerCase())) {
        throw data(
          "Machine names must be valid DNS labels: lowercase letters, numbers, and hyphens only, and must start and end with a letter or number.",
          { status: 400 },
        );
      }

      await api.nodes.rename(nodeId, name);
      await headscaleLiveStore.refresh(nodesResource, api);
      return { message: "Machine renamed" };
    }

    case "delete": {
      await api.nodes.delete(nodeId);
      await headscaleLiveStore.refresh(nodesResource, api);
      return redirect("/machines");
    }

    case "expire": {
      await api.nodes.expire(nodeId);
      await headscaleLiveStore.refresh(nodesResource, api);
      return { message: "Machine expired" };
    }

    case "update_tags": {
      const tags = formData.get("tags")?.toString().split(",") ?? [];
      if (tags.length === 0) {
        throw data("Missing `tags` in the form data.", {
          status: 400,
        });
      }

      try {
        await api.nodes.setTags(
          nodeId,
          tags.map((tag) => tag.trim()).filter((tag) => tag !== ""),
        );

        await headscaleLiveStore.refresh(nodesResource, api);
        return { success: true as const, message: "Tags updated" };
      } catch (error) {
        if (isDataWithApiError(error) && error.data.statusCode === 400) {
          return data(
            {
              success: false as const,
              error:
                extractApiErrorMessage(error.data) ??
                "One or more tags are not defined in your ACL policy. Please add them to your policy before assigning them to a machine.",
            },
            { status: 400 },
          );
        }

        throw error;
      }
    }

    case "update_routes": {
      const newApproved = node.approvedRoutes;
      const routes = formData.get("routes")?.toString();
      if (!routes) {
        throw data("Missing `routes` in the form data.", {
          status: 400,
        });
      }

      const allRoutes = routes.split(",").map((route) => route.trim());
      if (allRoutes.length === 0) {
        throw data("No routes provided to update", {
          status: 400,
        });
      }

      const enabled = formData.get("enabled")?.toString();
      if (enabled === undefined) {
        throw data("Missing `enabled` in the form data.", {
          status: 400,
        });
      }

      if (enabled === "true") {
        for (const route of allRoutes) {
          // If already approved, skip, otherwise add to approved
          if (newApproved.includes(route)) {
            continue;
          }

          newApproved.push(route);
        }
      } else {
        for (const route of allRoutes) {
          // If not approved, skip, otherwise remove from approved
          if (!newApproved.includes(route)) {
            continue;
          }

          const index = newApproved.indexOf(route);
          if (index > -1) {
            newApproved.splice(index, 1);
          }
        }
      }

      await api.nodes.approveRoutes(nodeId, newApproved);
      await headscaleLiveStore.refresh(nodesResource, api);
      return { message: "Routes updated" };
    }

    case "reassign": {
      const user = formData.get("user_id")?.toString();
      if (!user) {
        throw data("Missing `user_id` in the form data.", {
          status: 400,
        });
      }

      if (!api.nodes.reassignUser) {
        throw data("Reassigning a node owner is no longer supported on this Headscale version.", {
          status: 400,
        });
      }
      await api.nodes.reassignUser(nodeId, user);
      await headscaleLiveStore.refresh(nodesResource, api);
      return { message: "Machine reassigned" };
    }

    // Headplane-side override for a service's description shown on the
    // machine detail page. This doesn't touch Headscale/Tailscale at all —
    // it's purely local state layered on top of the read-only
    // `Hostinfo.Services` data at render time (see ~/server/service-overrides).
    case "update_service_description": {
      const proto = formData.get("proto")?.toString();
      const portRaw = formData.get("port")?.toString();
      const description = formData.get("description")?.toString() ?? "";
      if (!proto || !portRaw) {
        throw data("Missing `proto` or `port` in the form data.", {
          status: 400,
        });
      }

      const port = Number.parseInt(portRaw, 10);
      if (!Number.isFinite(port)) {
        throw data("Invalid `port` in the form data.", {
          status: 400,
        });
      }

      // "oidc" and "proxy" principals share the UserPrincipal shape (profile
      // + user); only "api_key" sessions have a plain displayName instead.
      const updatedBy =
        principal.kind === "api_key"
          ? principal.displayName
          : (principal.profile.name ?? principal.profile.username ?? principal.user.subject);

      await setServiceOverride(db, {
        hostId: node.nodeKey,
        proto,
        port,
        description,
        updatedBy,
      });

      return { message: description.trim() === "" ? "Description reset" : "Description updated" };
    }

    default:
      throw data("Invalid action", {
        status: 400,
      });
  }
}

function extractApiErrorMessage(error: { data?: unknown; rawData: string }) {
  if (error.data != null && typeof error.data === "object" && "message" in error.data) {
    const message = (error.data as { message?: unknown }).message;
    if (typeof message === "string" && message.length > 0) {
      return message;
    }
  }

  return error.rawData.length > 0 ? error.rawData : undefined;
}
