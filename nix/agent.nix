{buildGoModule}:
buildGoModule {
  pname = "hp_agent";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-oxZF0p+6Bsdw4OWsUfGed4hfFH6EFxpyFLZdvDkiirM=";
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
