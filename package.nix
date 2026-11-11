{
  lib,
  buildGoModule,
  ffmpeg-headless,
}:

let
  # transcribe decodes voice notes (Ogg/Opus, MP4, M4A…) with it.
  ffmpeg = lib.getExe ffmpeg-headless;
in
buildGoModule {
  pname = "matrix-bridge-mcp";
  version = "0.4.1";

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
    "-X main.version=0.4.1"
    "-X main.defaultFFmpeg=${ffmpeg}"
  ];

  # The tests run against an in-process homeserver stand-in and decode audio
  # with the same ffmpeg; building the package runs them.
  doCheck = true;
  preCheck = ''
    export PATH=${ffmpeg-headless}/bin:$PATH
  '';

  meta = {
    description = "A read-only MCP server over a Matrix homeserver's bridged chats";
    homepage = "https://github.com/daedalus-sh/matrix-bridge-mcp";
    license = lib.licenses.mit;
    mainProgram = "matrix-bridge-mcp";
  };
}
