{
  description = "A read-only MCP server over a Matrix homeserver's bridged chats";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      server = pkgs.callPackage ./package.nix { };
    in
    {
      packages.${system} = {
        default = server;
        matrix-bridge-mcp = server;
      };

      checks.${system} = {
        # Building the package runs the tests.
        default = server;
        lint =
          pkgs.runCommand "matrix-bridge-mcp-lint"
            {
              nativeBuildInputs = [
                pkgs.go
                pkgs.nixfmt
              ];
            }
            ''
              cd ${self}
              test -z "$(gofmt -l .)" || { gofmt -l .; exit 1; }
              nixfmt --check flake.nix package.nix
              touch $out
            '';
      };

      formatter.${system} = pkgs.nixfmt;
    };
}
