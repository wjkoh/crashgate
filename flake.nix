{
  description = "crashgate: Read-only deep-crash margin and tax-loss harvest watchdog for hledger portfolios";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = {
    self,
    nixpkgs,
  }: let
    supportedSystems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
    forEachSupportedSystem = f:
      nixpkgs.lib.genAttrs supportedSystems (system:
        f {
          pkgs = import nixpkgs {inherit system;};
          inherit system;
        });
  in {
    devShells = forEachSupportedSystem ({pkgs, ...}: {
      default = pkgs.mkShell {
        packages = with pkgs; [
          go
          go-tools
          go-task
          hledger
          git
        ];
        shellHook = ''
          if [ -d .git ] && [ -d .githooks ]; then
            git config core.hooksPath .githooks
          fi
        '';
      };
    });

    packages = forEachSupportedSystem ({pkgs, ...}: {
      default = pkgs.buildGoModule {
        pname = "crashgate";
        version = "0.1.0";
        src = ./.;
        vendorHash = null;
        env = {
          CGO_ENABLED = "0";
        };
        subPackages = ["cmd/crashgate"];
        ldflags = ["-s" "-w" "-X main.version=0.1.0"];
      };
    });

    checks = forEachSupportedSystem ({system, ...}: {
      package = self.packages.${system}.default;
    });
  };
}
