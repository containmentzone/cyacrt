# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac.enable = true;
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
in
{
  options.services.cyac = {
    enable = lib.mkEnableOption "the cyac runtime (daemon and proxy, CLI, kernel modules, sysctls, state dir)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "The cyac package. Its wrapper already carries runc, firecracker, iproute2, nftables and friends.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/cyac";
      description = "Where box records and disks live (CYAC_DIR).";
    };

    kernel = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Guest kernel (uncompressed vmlinux) for the VM tier, linked to
        stateDir/vmlinux. Without it, boxes run in the container tier.
      '';
    };

    proxy = {
      listen = lib.mkOption {
        type = lib.types.str;
        default = ":8080";
        description = "Plain HTTP listen address, used when acme is off.";
      };

      acme = lib.mkEnableOption ''
        Let's Encrypt for exposed hosts, serving on :80 and :443. Uses
        security.acme.defaults.email and requires security.acme.acceptTerms
      '';

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open TCP 80 and 443 (ACME mode only).";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];
    environment.variables.CYAC_DIR = cfg.stateDir;

    # loop: container-tier disks; tun: VM taps; bridge: cyac0.
    boot.kernelModules = [
      "loop"
      "tun"
      "bridge"
    ];
    # cyac sets this itself on start; declaring it keeps NixOS from fighting it.
    boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
    ]
    ++ lib.optional (cfg.kernel != null) "L+ ${cfg.stateDir}/vmlinux - - - - ${cfg.kernel}";

    # The daemon owns stateDir and serves the CLI's API on /run/cyac/cyac.sock
    # (root only, no other auth for now) and the proxy on the network.
    systemd.services.cyac = {
      description = "cyac daemon";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment.CYAC_DIR = cfg.stateDir;
      serviceConfig = {
        ExecStart =
          "${lib.getExe cfg.package} daemon "
          + (
            if cfg.proxy.acme then
              "--acme-email ${config.security.acme.defaults.email}"
            else
              "--listen ${cfg.proxy.listen}"
          );
        RuntimeDirectory = "cyac";
        RuntimeDirectoryMode = "0700";
        # Boxes are runc/firecracker children; stopping the daemon must not take them down.
        KillMode = "process";
        Restart = "on-failure";
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf (
      cfg.proxy.openFirewall && cfg.proxy.acme
    ) [ 80 443 ];

    assertions = lib.optionals cfg.proxy.acme [
      {
        assertion = config.security.acme.defaults.email != null;
        message = "services.cyac.proxy.acme needs security.acme.defaults.email.";
      }
      {
        assertion = config.security.acme.acceptTerms;
        message = "services.cyac.proxy.acme needs security.acme.acceptTerms = true.";
      }
    ];

    warnings = lib.optional (config.networking.nftables.enable && config.networking.nftables.flushRuleset) ''
      services.cyac: networking.nftables.flushRuleset wipes cyac's "inet cyac" table on every
      firewall reload, until the next `cyac start` puts it back. Consider flushRuleset = false.
    '';
  };
}
