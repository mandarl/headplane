{buildGoModule}:
buildGoModule {
  pname = "hp_agent";
  version = (builtins.fromJSON (builtins.readFile ../package.json)).version;
  src = ../.;
  vendorHash = "sha256-TkPS3eK7d7F/e2PDRs1w+kZWAcvlX333DnWSWutPq3g=";
  ldflags = ["-s" "-w"];
  env.CGO_ENABLED = 0;
}
