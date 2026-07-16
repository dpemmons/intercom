{
  description = "intercom — local-only agent messaging bridge for Claude Code and managed Codex";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      revision = self.rev or self.dirtyRev or "unknown";
      revisionLength = builtins.stringLength revision;
      shortRevision = builtins.substring 0 (if revisionLength < 8 then revisionLength else 8) revision;
      packageVersion = "0.2.4-${shortRevision}";
    in {
      packages = forAll (pkgs: rec {
        intercom = pkgs.buildGoModule {
          pname = "intercom";
          version = packageVersion;
          src = self;
          vendorHash = "sha256-DLNFiyt3TwHGFmBHk0Qg9HLYE4YrGmg15DpPX+/IG1M=";
          subPackages = [ "cmd/intercom" ];
          ldflags = [
            "-X main.version=${packageVersion}"
            "-X main.commit=${revision}"
          ];
          # go.mod pins go 1.25.5; use the matching toolchain from nixpkgs.
          go = pkgs.go_1_25;
          nativeBuildInputs = [ pkgs.makeWrapper ];
          nativeCheckInputs = pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.procps ];
          postInstall = ''
            install -Dm755 scripts/intercom-codex-project \
              "$out/bin/intercom-codex-project"
            substituteInPlace "$out/bin/intercom-codex-project" \
              --replace-fail '#!/usr/bin/env bash' '#!${pkgs.runtimeShell}' \
              --replace-fail 'intercom_bin=''${INTERCOM_BIN:-intercom}' \
                'intercom_bin=''${INTERCOM_BIN:-'"$out"'/bin/intercom}'
            wrapProgram "$out/bin/intercom-codex-project" \
              --prefix PATH : ${pkgs.lib.makeBinPath (
                [ pkgs.coreutils ] ++ pkgs.lib.optionals pkgs.stdenv.isLinux [ pkgs.procps ]
              )}
          '';
          meta = with pkgs.lib; {
            description = "Local messaging bridge for Claude Code and managed Codex sessions";
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

      checks = forAll (pkgs: {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.intercom;
      });
    };
}
