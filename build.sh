#!/bin/sh
# This is a general purpose build script for Headplane to be used across many
# different environments such as CI, Docker, Nix, and locally. For specific
# usage instructions, run `./build.sh --help`.

set -eu

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT_DIR" || exit 1

APP_DIR="$ROOT_DIR/app"
BUILD_DIR="$ROOT_DIR/build"
PUBLIC_DIR="$ROOT_DIR/public"

BUILD_WASM=0
BUILD_APP=0
BUILD_AGENT=0
BUILD_FAKE_SHELL=0
BUILD_HEALTHCHECK=0
BUILD_SERVER=0
BUILD_SPA=0
BUILD_SSR_PROXY=0
SKIP_PATH_CHECKS=0
SKIP_PNPM_PRUNE=0
APP_INSTALL_ONLY=0

WASM_OUTPUT="$PUBLIC_DIR/hp_ssh.wasm"
RDP_WASM_OUTPUT="$PUBLIC_DIR/hp_rdp.wasm"
AGENT_OUTPUT="$BUILD_DIR/hp_agent"
FAKE_SHELL_OUTPUT="$BUILD_DIR/hp_fake_sh"
HEALTHCHECK_OUTPUT="$BUILD_DIR/hp_healthcheck"
SERVER_OUTPUT="$BUILD_DIR/hp_server"

die() { echo "error: $*" >&2; exit 1; }
run() { echo ">> $*"; "$@"; }

while [ $# -gt 0 ]; do
	case "$1" in
		--wasm) BUILD_WASM=1 ;;
		--app) BUILD_APP=1 ;;
		--agent) BUILD_AGENT=1 ;;
		--fake-shell) BUILD_FAKE_SHELL=1 ;;
		--healthcheck) BUILD_HEALTHCHECK=1 ;;
		--server) BUILD_SERVER=1 ;;
		--spa) BUILD_SPA=1 ;;
		--ssr-proxy) BUILD_SSR_PROXY=1 ;;
		--interim) BUILD_SPA=1; BUILD_SSR_PROXY=1 ;;
		--skip-path-checks) SKIP_PATH_CHECKS=1 ;;
		--skip-pnpm-prune) SKIP_PNPM_PRUNE=1 ;;
		--app-install-only) APP_INSTALL_ONLY=1 ;;

		--wasm-output)
			shift
			[ $# -gt 0 ] || die "--wasm-output requires a path"
			WASM_OUTPUT=$1
			;;

		--rdp-wasm-output)
			shift
			[ $# -gt 0 ] || die "--rdp-wasm-output requires a path"
			RDP_WASM_OUTPUT=$1
			;;

		--agent-output)
			shift
			[ $# -gt 0 ] || die "--agent-output requires a path"
			AGENT_OUTPUT=$1
			;;

		--fake-shell-output)
			shift
			[ $# -gt 0 ] || die "--fake-shell-output requires a path"
			FAKE_SHELL_OUTPUT=$1
			;;

		--healthcheck-output)
			shift
			[ $# -gt 0 ] || die "--healthcheck-output requires a path"
			HEALTHCHECK_OUTPUT=$1
			;;

		--server-output)
			shift
			[ $# -gt 0 ] || die "--server-output requires a path"
			SERVER_OUTPUT=$1
			;;

		--help)
			cat <<EOF
Usage: $0 [flags]
  --wasm                       build wasm module
  --app                        build react-router app
  --spa                        build static SPA (ssr:false) into build/
  --ssr-proxy                  build SSR server for the interim proxy into build-ssr/
  --interim                    build both --ssr-proxy and --spa (Phase 0 interim)
  --agent                      build tailscale agent
  --fake-shell                 build fake shell binary (for Docker)
  --healthcheck                build healthcheck binary
  --server                     build Go API server (Phase 1+ skeleton)
  --skip-path-checks           skip safety checks (ie. checking PATH)
  --skip-pnpm-prune            skip pruning devDependencies from node_modules
  --app-install-only           only install app dependencies, skip build
  --wasm-output <path>         override wasm output path
  --rdp-wasm-output <path>     override RDP wasm output path
  --agent-output <path>        override agent output path
  --fake-shell-output <path>   override fake shell output path
  --healthcheck-output <path>  override healthcheck output path
EOF
			exit 0
			;;
		*)
			die "unknown flag: $1"
			;;
	esac
	shift
done

# By default build everything except for the fake shell
if [ "$BUILD_WASM" -eq 0 ] && [ "$BUILD_APP" -eq 0 ] && \
	[ "$BUILD_AGENT" -eq 0 ] && [ "$BUILD_FAKE_SHELL" -eq 0 ] && \
	[ "$BUILD_SPA" -eq 0 ] && [ "$BUILD_SSR_PROXY" -eq 0 ]; then
	BUILD_WASM=1
	BUILD_APP=1
	BUILD_AGENT=1
	BUILD_HEALTHCHECK=1
	BUILD_FAKE_SHELL=0
fi

if [ "$SKIP_PATH_CHECKS" -eq 0 ]; then
	[ -d "$ROOT_DIR" ] || die "missing project root"

	need_go=0
	need_pnpm=0

	[ "$BUILD_WASM" -eq 1 ] && need_go=1
	[ "$BUILD_APP" -eq 1 ] && need_pnpm=1
	[ "$BUILD_SPA" -eq 1 ] && need_pnpm=1
	[ "$BUILD_SSR_PROXY" -eq 1 ] && need_pnpm=1
	[ "$BUILD_AGENT" -eq 1 ] && need_go=1
	[ "$BUILD_FAKE_SHELL" -eq 1 ] && need_go=1
	[ "$BUILD_HEALTHCHECK" -eq 1 ] && need_go=1
	[ "$BUILD_SERVER" -eq 1 ] && need_go=1

	if [ $need_go -eq 1 ]; then
		echo "==> Checking for Go toolchain"
		command -v go >/dev/null 2>&1 || die "go not installed"
		go version >/dev/null 2>&1 || die "go not working"
	fi

	if [ $need_pnpm -eq 1 ]; then
		echo "==> Checking for node"
		command -v node >/dev/null 2>&1 || die "node not installed"
		node --version >/dev/null 2>&1 || die "node not working"

		echo "==> Checking for pnpm"
		command -v pnpm >/dev/null 2>&1 || die "pnpm not installed"
		pnpm --version >/dev/null 2>&1 || die "pnpm not working"
	fi
fi

build_wasm() {
	echo "==> Building SSH WASM module → $WASM_OUTPUT"
	mkdir -p "$(dirname "$WASM_OUTPUT")"
	echo "// $(go version)" > "$(dirname "$WASM_OUTPUT")/wasm_exec.js"

	# This depends on Go 1.23+ since the path is different in earlier versions
	cat "$(go env GOROOT)/lib/wasm/wasm_exec.js" >> \
		"$(dirname "$WASM_OUTPUT")/wasm_exec.js"

	# Vendor dependencies and apply the DERP port patch.
	# Tailscale's derphttp WebSocket URL builder ignores DERPPort,
	# which breaks WASM connections to non-443 DERP servers.
	echo "==> Vendoring Go dependencies for WASM patch"
	go mod vendor

	DERP_PATCH="$ROOT_DIR/patches/tailscale-derp-port.patch"
	if [ -f "$DERP_PATCH" ]; then
		echo "==> Applying DERP port patch"
		patch -d vendor/tailscale.com -p1 < "$DERP_PATCH" || \
			die "failed to apply DERP port patch"
	fi

	# gcc.go imports plugin only for three string constants; plugin has CGo
	# and is excluded by WASM build constraints. Inline the strings directly.
	GCC_FILE="vendor/github.com/tomatome/grdp/protocol/t125/gcc/gcc.go"
	if [ -f "$GCC_FILE" ]; then
		echo "==> Patching grdp gcc.go to remove plugin import"
		sed -i 's|"github.com/tomatome/grdp/plugin"||g' "$GCC_FILE"
		sed -i 's|plugin\.RDPDR_SVC_CHANNEL_NAME|"rdpdr"|g' "$GCC_FILE"
		sed -i 's|plugin\.RDPSND_SVC_CHANNEL_NAME|"rdpsnd"|g' "$GCC_FILE"
		sed -i 's|plugin\.CLIPRDR_SVC_CHANNEL_NAME|"cliprdr"|g' "$GCC_FILE"
	fi

	MCS_FILE="vendor/github.com/tomatome/grdp/protocol/t125/mcs.go"
	if [ -f "$MCS_FILE" ]; then
		echo "==> Patching grdp mcs.go to add SetColorDepth method"
		printf '\nfunc (c *MCSClient) SetColorDepth(depth uint16) {\n\tc.clientCoreData.HighColorDepth = gcc.HighColor(depth)\n}\n' >> "$MCS_FILE"
	fi

	GOOS=js GOARCH=wasm go build -mod=vendor \
		-ldflags="-s -w" -trimpath \
		-o "$WASM_OUTPUT" ./cmd/hp_ssh

	echo "==> Building RDP WASM module → $RDP_WASM_OUTPUT"
	mkdir -p "$(dirname "$RDP_WASM_OUTPUT")"
	GOOS=js GOARCH=wasm go build -mod=vendor \
		-ldflags="-s -w" -trimpath \
		-o "$RDP_WASM_OUTPUT" ./cmd/hp_rdp

	rm -rf vendor
}

build_app() {
	echo "==> Building React Router app → $BUILD_DIR"
	[ -f "$WASM_OUTPUT" ] || echo "warning: Building without SSH WASM module"
	[ -f "$RDP_WASM_OUTPUT" ] || echo "warning: Building without RDP WASM module"
	pnpm install --frozen-lockfile

	if [ "$APP_INSTALL_ONLY" -eq 1 ]; then
		echo "==> Skipping app build (install only)"
		return
	fi

	pnpm run build

	if [ "$SKIP_PNPM_PRUNE" -eq 0 ]; then
		echo "==> Pruning devDependencies from node_modules"
		pnpm prune --prod
	fi
}

build_agent() {
	echo "==> Building Tailscale agent → $AGENT_OUTPUT"
	mkdir -p "$(dirname "$AGENT_OUTPUT")"
	go build -o "$AGENT_OUTPUT" ./cmd/hp_agent
}

build_fake_shell() {
	[ -n "${IMAGE_TAG:-}" ] || die \
		"\$IMAGE_TAG is required to build fake shell binary"

	echo "==> Building fake shell binary → $FAKE_SHELL_OUTPUT"
	mkdir -p "$(dirname "$FAKE_SHELL_OUTPUT")"
	go build -ldflags="-s -w -X main.imageTag=${IMAGE_TAG}" \
		-o "$FAKE_SHELL_OUTPUT" ./cmd/fake_sh
}

build_healthcheck() {
	echo "==> Building healthcheck binary → $HEALTHCHECK_OUTPUT"
	mkdir -p "$(dirname "$HEALTHCHECK_OUTPUT")"
	go build -o "$HEALTHCHECK_OUTPUT" ./cmd/hp_healthcheck
}

# Phase 1+ (Go server): build the Go API server skeleton. It serves the SPA
# static bundle and /healthz; later phases add auth, OIDC, and the JSON API.
# headplaneVersion (for /api/info) comes from IMAGE_TAG when set, else "dev".
build_server() {
	echo "==> Building Go API server → $SERVER_OUTPUT"
	mkdir -p "$(dirname "$SERVER_OUTPUT")"
	go build -ldflags="-X main.headplaneVersion=${IMAGE_TAG:-dev}" \
		-o "$SERVER_OUTPUT" ./cmd/hp_server
}

# Phase 0 (SPA conversion): build the pruned SSR server that the interim
# reverse proxy uses for the server-driven flows (login POST, OIDC,
# SSH/RDP minting, /api/*). Output goes to build-ssr/server (not build/).
build_ssr_proxy() {
	echo "==> Building SSR proxy server → $ROOT_DIR/build-ssr/server"
	pnpm install --frozen-lockfile
	pnpm run build
	rm -rf "$ROOT_DIR/build-ssr"
	mkdir -p "$ROOT_DIR/build-ssr"
	mv "$BUILD_DIR/server" "$ROOT_DIR/build-ssr/server"
	echo "==> Run with: node $ROOT_DIR/build-ssr/server/index.js"
}

# Phase 0 (SPA conversion): build the static SPA (ssr:false) into build/.
build_spa() {
	echo "==> Building SPA (ssr:false) → $BUILD_DIR"
	pnpm install --frozen-lockfile
	HEADPLANE_SPA_BUILD=1 pnpm run build
	echo "==> Serve with: node $ROOT_DIR/server/interim.mjs"
}

[ "$BUILD_WASM" = 1 ] && build_wasm
[ "$BUILD_APP" = 1 ] && build_app
[ "$BUILD_AGENT" = 1 ] && build_agent
[ "$BUILD_FAKE_SHELL" = 1 ] && build_fake_shell
[ "$BUILD_HEALTHCHECK" = 1 ] && build_healthcheck
[ "$BUILD_SERVER" = 1 ] && build_server
# NOTE: the SSR proxy must build before the SPA — both use build/, and the
# SPA build overwrites build/client afterwards.
[ "$BUILD_SSR_PROXY" = 1 ] && build_ssr_proxy
[ "$BUILD_SPA" = 1 ] && build_spa

echo "✅ Build complete."
