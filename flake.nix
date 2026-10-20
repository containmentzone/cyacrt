{
  description = "cyacrt, the cyac runtime";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      forAllSystems = f: nixpkgs.lib.genAttrs [ "x86_64-linux" "aarch64-linux" ] (s: f nixpkgs.legacyPackages.${s});
    in
    {
      # The cyac binary, wrapped so every tool it shells out to is on its PATH.
      packages = forAllSystems (
        pkgs:
        rec {
          cyac = pkgs.callPackage ./nix/package.nix { };
          default = cyac;
        }
        # PVM kernels (see nix/pvm.nix); PVM is x86_64-only.
        // nixpkgs.lib.optionalAttrs (pkgs.stdenv.hostPlatform.system == "x86_64-linux") (
          let
            pvm = pkgs.callPackage ./nix/pvm.nix { };
          in
          {
            pvm-host-kernel = pvm.hostKernel;
            pvm-guest-kernel = pvm.guestKernel;
          }
        )
      );

      # Boots a box under firecracker-pvm in a VM with no VMX. Needs KVM.
      checks.x86_64-linux.pvm = import ./nix/test-pvm.nix {
        inherit self;
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          inputsFrom = [ self.packages.${pkgs.system}.cyac ];
          packages = [ pkgs.gopls ] ++ self.packages.${pkgs.system}.cyac.runtimeTools;
        };
      });

      # Host setup for NixOS: `imports = [ cyacrt.nixosModules.default ];`
      nixosModules.default = import ./nix/module.nix self;
    };
}
