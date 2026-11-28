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
          # The header-injecting proxy as an image for a cyservice's Image=.
          cyac-inject-image = pkgs.callPackage ./nix/inject.nix { inherit cyac; };
          # A Cloud Hypervisor guest vmlinux from nixpkgs' default kernel. On NixOS,
          # services.cyac.buildGuestKernel builds one from the host's kernel.
          guest-kernel = pkgs.callPackage ./nix/guest-kernel.nix { inherit (pkgs.linux) src version; };
        }
      );

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
