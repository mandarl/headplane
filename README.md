# Headplane

> A feature-complete web UI for [Headscale](https://headscale.net)

> [!IMPORTANT]
> **This is a hard fork — a port, not a tracking fork.** Upstream Headplane ships
> a Node / React Router SSR server. This fork keeps the React UI but **replaces
> the server with a small static Go binary** (`cmd/hp_server`, built
> `CGO_ENABLED=0`) that serves the app as a client-side SPA plus a JSON API under
> `/admin/api/v1/*`. The point is running Headplane on **extremely
> small-footprint machines** — for example a 1 GB GCE `e2-micro` that is already
> running Headscale and other services — where the Node runtime's memory cost is
> the binding constraint.
>
> Measured on an `e2-micro` (`bench/`): **idle RSS ~12 MiB vs ~130 MiB** for the
> Node build, ~13× lower under load, and no Node runtime in the image
> (`gcr.io/distroless/static`, `go-final` stage). Design, rationale, benchmarks,
> and cutover / rollback:
> [`docs/development/go-server-spa-rfc.md`](./docs/development/go-server-spa-rfc.md),
> [`docs/development/go-server-cutover.md`](./docs/development/go-server-cutover.md),
> [`docs/development/go-server-benchmark-results.md`](./docs/development/go-server-benchmark-results.md).
>
> Because the server tier is a reimplementation, this fork **does not merge
> upstream changes automatically** — the two codebases have diverged. It follows
> [`tale/headplane`](https://github.com/tale/headplane) for the React UI only, and
> that too is manual. If you want the maintained, upstream-mergeable version, use
> [`tale/headplane`](https://github.com/tale/headplane).

<picture>
    <source
        media="(prefers-color-scheme: dark)"
        srcset="./docs/assets/preview-dark.png"
    >
    <source
        media="(prefers-color-scheme: light)"
        srcset="./docs/assets/preview-light.png"
    >
    <img
        alt="Preview"
        src="./docs/assets/preview-dark.png"
    >
</picture>

Headscale is the de-facto self-hosted version of Tailscale, a popular Wireguard
based VPN service. By default, it does not ship with a web UI, which is where
Headplane comes in. Headplane is a feature-complete web UI for Headscale, allowing
you to manage your nodes, networks, and ACLs with ease.

Headplane aims to replicate the functionality offered by the official Tailscale
product and dashboard, being one of the most feature complete Headscale UIs available.
These are some of the features that Headplane offers:

- Machine management, including expiry, network routing, name, and owner management
- Access Control List (ACL) and tagging configuration for ACL enforcement
- Support for OpenID Connect (OIDC) as a login provider
- The ability to edit DNS settings and automatically provision Headscale
- Configurability for Headscale's settings
- Browser SSH and RDP terminals (xterm.js + WASM), unchanged from upstream

## Architecture (this fork)

| | Upstream | This fork |
| --- | --- | --- |
| Server | `node build/server/index.js` (React SSR per request) | `hp_server` — static Go binary, `CGO_ENABLED=0` |
| UI delivery | React SSR | Same React app as a static SPA (`build/client`) |
| Data | React Router loaders/actions (server-side) | JSON API `/admin/api/v1/*` + `/admin/events/live` SSE |
| Docker image | `final` stage (distroless Node) | `go-final` stage (`distroless/static`) |
| Config file, `hp_persist.db`, `_hp_auth` cookie | — | **Byte-identical** — cut over or roll back with no data migration |

The Node `final` image and Nix `headplane` package are still built, as the
one-step rollback path.

## Deployment

Refer to the [website](https://headplane.net) for detailed installation
instructions of upstream Headplane; the configuration surface (YAML file,
`HEADPLANE_*` env vars, `*_path` secrets) is identical. For this fork's Go image,
see [`docs/development/go-server-cutover.md`](./docs/development/go-server-cutover.md).

## Versioning

Headplane uses [semantic versioning](https://semver.org/) for its releases (since v0.6.0).
Pre-release builds are available under the `next` tag and get updated when a new release
PR is opened and actively in testing. This fork's Go images are published with a
`-go` suffix (e.g. `:<version>-go`).

## Contributing

Headplane is an open-source project and contributions are welcome! If you have
any suggestions, bug reports, or feature requests, please open an issue. Also
refer to the [contributor guidelines](./docs/CONTRIBUTING.md) for more info.

---

<picture>
    <source
        media="(prefers-color-scheme: dark)"
        srcset="./docs/assets/acls-dark.png"
    >
    <source
        media="(prefers-color-scheme: light)"
        srcset="./docs/assets/acls-light.png"
    >
    <img
        alt="ACLs"
        src="./docs/assets/acls-dark.png"
    >
</picture>

<picture>
    <source
        media="(prefers-color-scheme: dark)"
        srcset="./docs/assets/machine-dark.png"
    >
    <source
        media="(prefers-color-scheme: light)"
        srcset="./docs/assets/machine-light.png"
    >
    <img
        alt="Machine Management"
        src="./docs/assets/machine-dark.png"
    >
</picture>

> Upstream Headplane by Aarnav Tale — <https://github.com/tale/headplane>.
> This fork's Go server + SPA port is maintained separately.
>
> Copyright (c) 2025 Aarnav Tale
