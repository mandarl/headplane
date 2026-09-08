{buildGoModule}:
buildGoModule {
  pname = "hp_agent";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-6e6GtHV+wA5Arkmv+3YgsIWNiW40JM2W4cATL+Bk02s=";
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
