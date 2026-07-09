{
  description = "intercom — local-only chat bridge between Claude Code sessions (MCP shim + broker)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in {
      packages = forAll (pkgs: rec {
        intercom = pkgs.buildGoModule {
          pname = "intercom";
          version = "0.1.0-${builtins.substring 0 8 (self.rev or self.dirtyRev or "dev")}";
          src = self;
          vendorHash = "sha256-7K17JaXFsjf163g5PXCb5ng2gYdotnZ2IDKk8KFjNj0=";
          subPackages = [ "cmd/intercom" ];
          # go.mod pins go 1.25.5; use the matching toolchain from nixpkgs.
          go = pkgs.go_1_25;
          meta = with pkgs.lib; {
            description = "Local chat bridge between Claude Code sessions";
            homepage = "https://git.dpemmons.com/dpemmons/intercom";
            license = licenses.mit;
            mainProgram = "intercom";
          };
        };
        default = intercom;
      });

      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go_1_25 pkgs.gopls pkgs.gotools ];
        };
      });

      apps = forAll (pkgs: {
        default = {
          type = "app";
          program = nixpkgs.lib.getExe self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        };
      });
    };
}
