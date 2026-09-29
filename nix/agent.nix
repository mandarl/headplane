{buildGoModule}:
buildGoModule {
  pname = "hp_agent";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-SVJid7ddbJ7Srwlsy02iPcclSXRbDUImILsWjzg8Mu8=";
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
