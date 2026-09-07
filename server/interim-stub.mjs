// MARK: Phase 0 interim stub API
//
// Stub implementation of the `/admin/api/v1/*` JSON API contract, served by
// `server/interim.mjs` while the React app runs as an SPA against Node.
// Every response shape follows the routes contract (see the Phase 0 commit
// message); the data itself is fixed fixtures — clearly marked as such.
//
// A stub session cookie (`_hp_stub=1`, set via POST /stub/session) stands in
// for real auth: all other v1 endpoints 401 without it, exactly like the
// real API does without a session. This file is deleted when the Go server
// implements the API for real (Phase 4/5).

const STUB_COOKIE = "_hp_stub";

const LOGIN_CONFIG = {
  oidcEnabled: true,
  oidcErrorCodes: [],
  cookieSecure: false,
  disableApiKeyLogin: false,
};

const STUB_USERS = [
  { id: "1", name: "alice", createdAt: "2026-01-15T10:00:00Z" },
  { id: "2", name: "bob", createdAt: "2026-02-20T10:00:00Z" },
];

function stubNode(id, overrides = {}) {
  return {
    id,
    machineKey: `mkey:stub-machine-key-${id}`,
    nodeKey: `nodekey:stub-node-key-${id}`,
    discoKey: `discokey:stub-disco-key-${id}`,
    ipAddresses: ["100.64.0.1", "fd7a:db5:21:0:0:0:0:1"],
    name: `node${id}.stub.example.com`,
    user: STUB_USERS[0],
    lastSeen: "2026-09-07T11:55:00Z",
    expiry: null,
    createdAt: "2026-01-15T10:00:00Z",
    registerMethod: "REGISTER_METHOD_AUTH_KEY",
    tags: [],
    givenName: `node${id}`,
    online: true,
    approvedRoutes: [],
    availableRoutes: [],
    subnetRoutes: [],
    ...overrides,
  };
}

function stubHostInfo() {
  return {
    IPNVersion: "1.88.0",
    OS: "linux",
    OSVersion: "6.8.0-52-generic",
    Hostname: "node1",
    Services: [{ Proto: "tcp", Port: 80, Description: "stub web server" }],
    NetInfo: {
      MappingVariesByDestIP: false,
      HairPinning: false,
      WorkingIPv6: true,
      WorkingUDP: true,
      UPnP: false,
      PCP: false,
      PMP: false,
    },
    IngressEnabled: false,
    Endpoints: ["192.0.2.10:41641"],
  };
}

function stubPopulatedNode(node) {
  return {
    ...node,
    routes: [],
    hostInfo: stubHostInfo(),
    expired: false,
    customRouting: {
      exitRoutes: [],
      exitApproved: true,
      subnetApprovedRoutes: [],
      subnetWaitingRoutes: [],
    },
  };
}

const STUB_ACL_POLICY = `// Stub ACL policy served by the Phase 0 interim server.
{
\t"groups": {"group:admins": ["alice@example.com"]},
\t"tagOwners": {"tag:server": ["group:admins"]},
\t"hosts": {"node1": "100.64.0.1"},
\t"acls": [{"action": "accept", "src": ["group:admins"], "dst": ["*:*"]}],
}
`;

function parseCookies(req) {
  const header = req.headers.cookie ?? "";
  const out = {};
  for (const part of header.split(";")) {
    const idx = part.indexOf("=");
    if (idx > 0) {
      out[part.slice(0, idx).trim()] = decodeURIComponent(part.slice(idx + 1).trim());
    }
  }
  return out;
}

function readJsonBody(req) {
  return new Promise((resolve) => {
    let raw = "";
    req.on("data", (chunk) => {
      raw += chunk;
    });
    req.on("end", () => {
      if (!raw) {
        resolve({});
        return;
      }
      try {
        resolve(JSON.parse(raw));
      } catch {
        resolve(null);
      }
    });
  });
}

function json(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(payload),
  });
  res.end(payload);
}

function unauthorized(res) {
  json(res, 401, { error: "Unauthenticated", login: LOGIN_CONFIG });
}

const MACHINE_ACTIONS = {
  register: (b) =>
    !b.register_key || !b.user
      ? [400, { error: "Missing required fields" }]
      : [200, { redirect: "/machines/1" }],
  rename: (b) =>
    !b.node_id || !b.name
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Machine renamed" }],
  delete: (b) =>
    !b.node_id ? [400, { error: "Missing required fields" }] : [200, { redirect: "/machines" }],
  expire: (b) =>
    !b.node_id
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Machine expired" }],
  update_tags: (b) => {
    const tags = Array.isArray(b.tags) ? b.tags : String(b.tags ?? "").split(",");
    if (!b.node_id) return [400, { error: "Missing required fields" }];
    if (tags.some((t) => t.trim() === "tag:invalid")) {
      return [
        400,
        {
          success: false,
          error:
            "One or more tags are not defined in your ACL policy. Define them under tagOwners first.",
        },
      ];
    }
    return [200, { success: true, message: "Tags updated" }];
  },
  update_routes: (b) =>
    !b.node_id || b.routes === undefined || b.enabled === undefined
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Routes updated" }],
  reassign: (b) =>
    !b.node_id || !b.user_id
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Machine reassigned" }],
  update_service_description: (b) =>
    !b.node_id || !b.proto || b.port === undefined || b.description === undefined
      ? [400, { error: "Missing required fields" }]
      : [200, { message: b.description ? "Description updated" : "Description reset" }],
};

const USER_ACTIONS = {
  create_user: (b) =>
    !b.username
      ? [400, { error: "Missing username" }]
      : [200, { message: "User created successfully" }],
  delete_user: (b) =>
    !b.headscale_user_id
      ? [400, { error: "Missing headscale_user_id" }]
      : [200, { message: "User deleted successfully" }],
  rename_user: (b) =>
    !b.headscale_user_id || !b.new_name
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "User renamed successfully" }],
  reassign_user: (b) =>
    !b.headplane_user_id || !b.new_role
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "User reassigned successfully" }],
  transfer_ownership: (b) =>
    !b.headplane_user_id
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Ownership transferred successfully" }],
  link_user: (b) =>
    !b.headplane_user_id || !b.headscale_user_id
      ? [400, { error: "Missing required fields" }]
      : [200, { message: "Headscale user linked successfully" }],
};

const DNS_ACTIONS = {
  rename_tailnet: (b) =>
    !b.new_name ? [400, { success: false }] : [200, { message: "Tailnet renamed successfully" }],
  toggle_magic: (b) =>
    !b.new_state
      ? [400, { success: false }]
      : [200, { message: "Magic DNS state updated successfully" }],
  add_ns: (b) =>
    !b.ns || !b.split_name ? [400, { success: false }] : [200, { message: "Nameserver added" }],
  remove_ns: (b) =>
    !b.ns || !b.split_name ? [400, { success: false }] : [200, { message: "Nameserver removed" }],
  add_domain: (b) =>
    !b.domain ? [400, { success: false }] : [200, { message: "Search domain added" }],
  remove_domain: (b) =>
    !b.domain ? [400, { success: false }] : [200, { message: "Search domain removed" }],
  add_record: (b) =>
    !b.record_name || !b.record_type || !b.record_value ? [400, { success: false }] : [200, {}],
  remove_record: (b) => (!b.record_name || !b.record_type ? [400, { success: false }] : [200, {}]),
  override_dns: (b) =>
    b.override_dns === undefined
      ? [400, { success: false }]
      : [200, { message: "DNS override updated" }],
};

const AUTHKEY_ACTIONS = {
  add_preauthkey: (b) => {
    if (!b.expiry || b.reusable === undefined || b.ephemeral === undefined) {
      return [400, { error: "Missing required fields" }];
    }
    if (!b.user_id && !(b.acl_tags && b.acl_tags.length)) {
      return [400, { error: "Must specify either a user or ACL tags." }];
    }
    return [200, { success: true, key: "hp_stub_preauthkey_secret_shown_once" }];
  },
  expire_preauthkey: (b) =>
    !b.key_id || !b.key || !b.user_id
      ? [400, { error: "Missing required fields" }]
      : [200, "Pre-auth key expired"],
};

const RESTRICTION_ACTIONS = {
  add_domain: (b) =>
    !b.domain ? [400, { error: "Missing domain" }] : [200, "Domain added successfully."],
  remove_domain: (b) =>
    !b.domain ? [400, { error: "Missing domain" }] : [200, "Domain removed successfully."],
  add_group: (b) =>
    !b.group ? [400, { error: "Missing group" }] : [200, "Group added successfully."],
  remove_group: (b) =>
    !b.group ? [400, { error: "Missing group" }] : [200, "Group removed successfully."],
  add_user: (b) => (!b.user ? [400, { error: "Missing user" }] : [200, "User added successfully."]),
  remove_user: (b) =>
    !b.user ? [400, { error: "Missing user" }] : [200, "User removed successfully."],
};

function runAction(table, body) {
  const fn = table[body.action_id];
  if (!fn) return [400, { success: false, error: `Unknown action_id: ${body.action_id}` }];
  return fn(body);
}

/**
 * Handles `/admin/api/v1/*`. `subpath` is the path after the `/api/v1`
 * prefix (always starts with `/`). Returns true when the request was handled.
 */
export async function handleStubRequest(req, res, subpath) {
  const method = req.method ?? "GET";
  const cookies = parseCookies(req);

  // Stub session management (interim-only; stands in for the real session).
  if (subpath === "/stub/session") {
    if (method === "POST") {
      const body = await readJsonBody(req);
      if (!body || body.key !== "stub") {
        json(res, 400, { error: 'POST {"key":"stub"} to create a stub session' });
        return true;
      }
      res.writeHead(200, {
        "Content-Type": "application/json",
        "Set-Cookie": `${STUB_COOKIE}=1; Path=/admin; SameSite=Lax; HttpOnly`,
      });
      res.end(JSON.stringify({ ok: true }));
      return true;
    }
    if (method === "DELETE") {
      res.writeHead(200, {
        "Content-Type": "application/json",
        "Set-Cookie": `${STUB_COOKIE}=; Path=/admin; Max-Age=0; SameSite=Lax; HttpOnly`,
      });
      res.end(JSON.stringify({ ok: true }));
      return true;
    }
    json(res, 405, { error: "Method not allowed" });
    return true;
  }

  if (cookies[STUB_COOKIE] !== "1") {
    unauthorized(res);
    return true;
  }

  const body = method === "GET" || method === "HEAD" ? {} : await readJsonBody(req);
  if (body === null) {
    json(res, 400, { error: "Invalid JSON body" });
    return true;
  }

  // --- boot ---
  if (subpath === "/boot" && method === "GET") {
    json(res, 200, {
      user: {
        kind: "oidc",
        email: "admin@example.com",
        name: "Stub Admin",
        picture: null,
        subject: "stub-subject",
        username: "admin",
        headscaleUserId: "1",
        role: "owner",
      },
      access: { dns: true, machines: true, policy: true, settings: true, ui: true, users: true },
      baseUrl: "https://headscale.example.com",
      configAvailable: true,
      isHealthy: true,
      isDebug: false,
    });
    return true;
  }

  // --- home ---
  if (subpath === "/home" && method === "GET") {
    json(res, 200, {
      status: "needs_link",
      headscaleUsers: [
        { id: "1", name: "alice" },
        { id: "2", name: "bob" },
      ],
    });
    return true;
  }
  if (subpath === "/home/link" && method === "POST") {
    json(res, 200, { redirect: "/" });
    return true;
  }

  // --- machines ---
  if (subpath === "/machines" && method === "GET") {
    const nodes = [
      stubNode("1"),
      stubNode("2", {
        name: "server1.stub.example.com",
        givenName: "server1",
        user: null,
        tags: ["tag:server"],
        online: false,
        lastSeen: "2026-09-06T08:00:00Z",
        ipAddresses: ["100.64.0.2"],
        availableRoutes: ["10.0.0.0/8"],
        approvedRoutes: ["10.0.0.0/8"],
        subnetRoutes: ["10.0.0.0/8"],
        expiry: "2026-12-31T00:00:00Z",
      }),
    ];
    json(res, 200, {
      agent: { syncedAt: "2026-09-07T11:50:00Z", nodeCount: 2, nodeKey: "nodekey:stub-node-key-1" },
      headscaleUserId: "1",
      magic: "stub.example.com",
      nodes,
      populatedNodes: nodes.map(stubPopulatedNode),
      preAuth: true,
      publicServer: "https://headscale.example.com",
      server: "https://headscale.example.com",
      supportsNodeOwnerChange: true,
      users: STUB_USERS,
      writable: true,
    });
    return true;
  }
  {
    const m = subpath.match(/^\/machines\/([^/]+)$/);
    if (m && method === "GET") {
      const node = stubPopulatedNode(stubNode(m[1]));
      json(res, 200, {
        agent: {
          syncedAt: "2026-09-07T11:50:00Z",
          nodeCount: 2,
          nodeKey: "nodekey:stub-node-key-1",
        },
        existingTags: ["tag:server"],
        magic: "stub.example.com",
        node,
        rdpGatewayEnabled: false,
        serviceOverrides: {},
        stats: stubHostInfo(),
        supportsNodeOwnerChange: true,
        tags: node.tags,
        users: STUB_USERS,
      });
      return true;
    }
  }
  if (subpath === "/machines/actions" && method === "POST") {
    const [status, payload] = runAction(MACHINE_ACTIONS, body);
    json(res, status, payload);
    return true;
  }

  // --- users ---
  if (subpath === "/users" && method === "GET") {
    json(res, 200, {
      writable: true,
      currentUserId: "hp-stub-user-1",
      isOwner: true,
      oidc: { issuer: "https://issuer.example.com" },
      magic: "stub.example.com",
      apiError: null,
      headplaneUsers: [
        {
          id: "hp-stub-user-1",
          sub: "stub-subject",
          name: "Stub Admin",
          email: "admin@example.com",
          role: "owner",
          headscaleUserId: "1",
          createdAt: "2026-01-15T10:00:00Z",
          lastLoginAt: "2026-09-07T11:55:00Z",
          machines: [],
        },
      ],
      unlinkedHeadscaleUsers: [{ ...STUB_USERS[1], machines: [] }],
      headscaleUsersForLink: STUB_USERS.map((u) => ({ id: u.id, name: u.name, claimed: true })),
    });
    return true;
  }
  if (subpath === "/users/actions" && method === "POST") {
    const [status, payload] = runAction(USER_ACTIONS, body);
    json(res, status, payload);
    return true;
  }

  // --- acls ---
  if (subpath === "/acls" && method === "GET") {
    json(res, 200, { access: true, writable: true, policy: STUB_ACL_POLICY });
    return true;
  }
  if (subpath === "/acls" && method === "PATCH") {
    if (typeof body.policy === "string" && body.policy.includes("SYNTAX_ERROR")) {
      json(res, 400, {
        success: false,
        error: "Syntax error: stub-detected SYNTAX_ERROR marker",
        policy: null,
        updatedAt: null,
      });
      return true;
    }
    if (typeof body.policy !== "string") {
      json(res, 400, { error: "Missing policy" });
      return true;
    }
    json(res, 200, {
      success: true,
      error: null,
      policy: body.policy,
      updatedAt: "2026-09-07T12:00:00Z",
    });
    return true;
  }

  // --- dns ---
  if (subpath === "/dns" && method === "GET") {
    json(res, 200, {
      prefixes: ["100.64.0.0/10", "fd7a:db5:21::/48"],
      magicDns: true,
      baseDomain: "stub.example.com",
      nameservers: ["1.1.1.1"],
      splitDns: { "example.com": ["9.9.9.9"] },
      searchDomains: ["example.com"],
      overrideDns: false,
      extraRecords: [{ name: "test", type: "A", value: "192.0.2.1" }],
      access: true,
      writable: true,
    });
    return true;
  }
  if (subpath === "/dns/actions" && method === "POST") {
    const [status, payload] = runAction(DNS_ACTIONS, body);
    json(res, status, payload);
    return true;
  }

  // --- settings ---
  if (subpath === "/settings" && method === "GET") {
    json(res, 200, { config: true, isOidcEnabled: true });
    return true;
  }
  if (subpath === "/settings/auth-keys" && method === "GET") {
    json(res, 200, {
      access: true,
      currentSubject: "stub-subject",
      keys: [
        {
          user: STUB_USERS[0],
          preAuthKeys: [
            {
              id: "1",
              key: "hp_stubkey1",
              user: STUB_USERS[0],
              reusable: true,
              ephemeral: false,
              used: false,
              expiration: "2026-12-31T00:00:00Z",
              createdAt: "2026-09-01T00:00:00Z",
              aclTags: [],
            },
          ],
        },
        {
          user: null,
          preAuthKeys: [
            {
              id: "2",
              key: "hp_stubkey2",
              user: null,
              reusable: false,
              ephemeral: true,
              used: false,
              expiration: "2026-10-07T00:00:00Z",
              createdAt: "2026-09-06T00:00:00Z",
              aclTags: ["tag:ci"],
            },
          ],
        },
      ],
      missing: [],
      selfServiceOnly: false,
      url: "https://headscale.example.com",
      users: STUB_USERS,
    });
    return true;
  }
  if (subpath === "/settings/auth-keys/actions" && method === "POST") {
    const [status, payload] = runAction(AUTHKEY_ACTIONS, body);
    json(res, status, payload);
    return true;
  }
  if (subpath === "/settings/restrictions" && method === "GET") {
    json(res, 200, {
      access: true,
      settings: { domains: ["example.com"], groups: [], users: [] },
      writable: true,
    });
    return true;
  }
  if (subpath === "/settings/restrictions/actions" && method === "POST") {
    const [status, payload] = runAction(RESTRICTION_ACTIONS, body);
    json(res, status, payload);
    return true;
  }
  if (subpath === "/settings/agent" && method === "GET") {
    json(res, 200, {
      enabled: true,
      syncedAt: "2026-09-07T11:50:00Z",
      nodeCount: 2,
      error: null,
    });
    return true;
  }
  if (subpath === "/settings/agent/sync" && method === "POST") {
    json(res, 200, { success: true, error: null });
    return true;
  }

  json(res, 404, { error: `Unknown stub endpoint: ${method} ${subpath}` });
  return true;
}
