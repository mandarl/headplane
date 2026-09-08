FROM --platform=$BUILDPLATFORM golang:1.25.1 AS go-base
WORKDIR /run
RUN apt-get update && apt-get install -y --no-install-recommends patch && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum build.sh ./
COPY patches/ ./patches/
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/
# cmd/hp_server imports the root dbmigrate package, which embeds drizzle/.
COPY dbmigrate.go ./
COPY drizzle/ ./drizzle/

ARG TARGETOS
ARG TARGETARCH
ARG IMAGE_TAG
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 IMAGE_TAG=$IMAGE_TAG \
	./build.sh --wasm --agent --fake-shell --healthcheck \
		--server \
		--wasm-output /bin/hp_ssh.wasm \
		--rdp-wasm-output /bin/hp_rdp.wasm \
		--agent-output /bin/hp_agent \
		--fake-shell-output /bin/fake-sh \
		--healthcheck-output /bin/hp_healthcheck \
		--server-output /bin/hp_server

RUN chmod +x /bin/hp_ssh.wasm
RUN chmod +x /bin/hp_rdp.wasm
RUN chmod +x /bin/hp_agent
RUN chmod +x /bin/fake-sh
RUN chmod +x /bin/hp_healthcheck
RUN chmod +x /bin/hp_server

# Folder needs to exist for later stages
RUN mkdir -p /var/lib/headplane/agent

FROM --platform=$BUILDPLATFORM node:24-slim AS js-base
WORKDIR /run

RUN corepack enable
COPY patches ./patches
COPY package.json pnpm-lock.yaml build.sh ./

COPY --from=go-base /bin/hp_ssh.wasm /run/public/hp_ssh.wasm
COPY --from=go-base /bin/hp_rdp.wasm /run/public/hp_rdp.wasm
COPY --from=go-base /bin/wasm_exec.js /run/public/wasm_exec.js
RUN ./build.sh --app --app-install-only

COPY . .
ARG HEADPLANE_VERSION
RUN HEADPLANE_VERSION=$HEADPLANE_VERSION ./build.sh --app

# SPA bundle for the Go server: the same `build/client` tree that
# `hp_server` serves by default (see --client-dir).
FROM --platform=$BUILDPLATFORM node:24-slim AS spa-base
WORKDIR /run

RUN corepack enable
# Full source tree: the SPA build needs app/, public/, configs, etc.
COPY . ./

COPY --from=go-base /bin/hp_ssh.wasm /run/public/hp_ssh.wasm
COPY --from=go-base /bin/hp_rdp.wasm /run/public/hp_rdp.wasm
COPY --from=go-base /bin/wasm_exec.js /run/public/wasm_exec.js
RUN ./build.sh --spa --skip-pnpm-prune --skip-path-checks

FROM gcr.io/distroless/nodejs24-debian13:latest AS final
COPY --from=js-base /run/build /app/build
COPY --from=js-base /run/drizzle /app/drizzle

COPY --from=go-base /bin/hp_agent /usr/libexec/headplane/agent
COPY --from=go-base /var/lib/headplane /var/lib/headplane

# Fake shell to inform the user that they should use the debug image
COPY --from=go-base /bin/fake-sh /bin/sh
COPY --from=go-base /bin/fake-sh /bin/bash

COPY --from=go-base /bin/hp_healthcheck /bin/hp_healthcheck
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD ["/bin/hp_healthcheck"]

# Tells Headplane to publish its loopback healthcheck URL to this
# file on startup; `hp_healthcheck` reads it. Docker-only — native
# installs don't ship a consumer.
ENV HEADPLANE_LISTEN_FILE=/tmp/headplane-listen

WORKDIR /app
CMD [ "/app/build/server/index.js" ]

FROM node:24-alpine AS debug-shell
RUN apk add --no-cache bash curl

COPY --from=js-base /run/build /app/build
COPY --from=js-base /run/drizzle /app/drizzle

COPY --from=go-base /bin/hp_agent /usr/libexec/headplane/agent
COPY --from=go-base /var/lib/headplane /var/lib/headplane
COPY --from=go-base /bin/hp_healthcheck /bin/hp_healthcheck

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD ["/bin/hp_healthcheck"]

ENV HEADPLANE_LISTEN_FILE=/tmp/headplane-listen

WORKDIR /app
CMD [ "node", "/app/build/server/index.js" ]

# Phase 6: the Go API server image. All binaries are CGO_ENABLED=0 static
# builds, so distroless/static suffices — and it already ships the CA
# bundle OIDC and Headscale TLS need (scratch would require adding one
# manually). The SPA bundle lives at /app/build/client, which is
# hp_server's default --client-dir relative to WORKDIR /app.
# Config: mount a config file at /etc/headplane/config.yaml or set
# HEADPLANE_CONFIG_PATH, exactly like the Node image.
FROM gcr.io/distroless/static-debian13:latest AS go-final
COPY --from=spa-base /run/build/client /app/build/client
COPY --from=go-base /bin/hp_server /usr/local/bin/hp_server

COPY --from=go-base /bin/hp_agent /usr/libexec/headplane/agent
COPY --from=go-base /var/lib/headplane /var/lib/headplane

# Fake shell to inform the user that they should use the debug image
COPY --from=go-base /bin/fake-sh /bin/sh
COPY --from=go-base /bin/fake-sh /bin/bash

COPY --from=go-base /bin/hp_healthcheck /bin/hp_healthcheck
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD ["/bin/hp_healthcheck"]

# Tells Headplane to publish its loopback healthcheck URL to this
# file on startup; `hp_healthcheck` reads it. Docker-only — native
# installs don't ship a consumer.
ENV HEADPLANE_LISTEN_FILE=/tmp/headplane-listen

WORKDIR /app
ENTRYPOINT [ "/usr/local/bin/hp_server" ]

# Debug variant of the Go image with a real shell and curl.
FROM debian:13-slim AS go-debug
RUN apt-get update && apt-get install -y --no-install-recommends \
		bash curl ca-certificates && rm -rf /var/lib/apt/lists/*

COPY --from=spa-base /run/build/client /app/build/client
COPY --from=go-base /bin/hp_server /usr/local/bin/hp_server
COPY --from=go-base /bin/hp_agent /usr/libexec/headplane/agent
COPY --from=go-base /var/lib/headplane /var/lib/headplane
COPY --from=go-base /bin/hp_healthcheck /bin/hp_healthcheck

HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
	CMD ["/bin/hp_healthcheck"]

ENV HEADPLANE_LISTEN_FILE=/tmp/headplane-listen

WORKDIR /app
ENTRYPOINT [ "/usr/local/bin/hp_server" ]
