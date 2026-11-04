{ lib, buildGoModule }:

buildGoModule {
  pname = "matrix-bridge-mcp";
  version = "0.2.1";

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./go.sum
      (lib.fileset.fileFilter (f: f.hasExt "go") ./.)
    ];
  };

  vendorHash = "sha256-NbuvbsmNbz3aKdjyyfpkbi52yRhXl473j3LvAUDn3L8=";

  env.CGO_ENABLED = "0";
  ldflags = [
    "-s"
    "-w"
    "-X main.version=0.2.1"
  ];

  # The tests run against an in-process homeserver stand-in; building the
  # package runs them.
  doCheck = true;

  meta = {
    description = "A read-only MCP server over a Matrix homeserver's bridged chats";
    homepage = "https://github.com/santiagotoscanini/matrix-bridge-mcp";
    license = lib.licenses.mit;
    mainProgram = "matrix-bridge-mcp";
  };
}
