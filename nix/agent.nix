{buildGoModule}:
buildGoModule {
  pname = "hp_agent";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-phMNjw9U7vdFpPF9e+olpD+UoEsJfy8ilGaprubNdNc=";
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
