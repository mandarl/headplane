# Phase 6: Nix package for the Go API server (cmd/hp_server).
#
# This is a pure-Go derivation mirroring nix/agent.nix: same go.mod, same
# vendorHash, CGO_ENABLED=0. It intentionally does NOT bundle the SPA
# static assets — those come from the `headplane` package's
# share/headplane/build/client tree (built by nix/package.nix's SPA-capable
# pnpm build); point hp_server at it with --client-dir.
{buildGoModule}:
buildGoModule {
  pname = "headplane-go";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-SVJid7ddbJ7Srwlsy02iPcclSXRbDUImILsWjzg8Mu8=";
  subPackages = ["cmd/hp_server"];
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
