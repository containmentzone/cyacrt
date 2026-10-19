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
  oidc = cfg.serve.oidc;
in
{
  options.services.cyac = {
    enable = lib.mkEnableOption "the cyac runtime (CLI, kernel modules, sysctls, state dir)";

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

    serve = {
      enable = lib.mkEnableOption "the cyac proxy as a systemd service";

      listen = lib.mkOption {
        type = lib.types.str;
        default = ":8080";
        description = "Plain HTTP listen address, used when acmeEmail is null.";
      };

      acmeEmail = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = null;
        description = "Enables Let's Encrypt for exposed hosts and serves on :80 and :443.";
      };

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open TCP 80 and 443 (ACME mode only).";
      };

      oidc = {
        issuer = lib.mkOption {
          type = lib.types.nullOr lib.types.str;
          default = null;
          example = "https://id.example.com";
          description = "OIDC issuer URL. Setting it lets allowed users sign in to non-public routes; route tokens keep working.";
        };

        clientId = lib.mkOption {
          type = lib.types.str;
          default = "";
          description = "OIDC client ID.";
        };

        clientSecretFile = lib.mkOption {
          type = lib.types.nullOr lib.types.path;
          default = null;
          description = "File holding the OIDC client secret, passed to the service with LoadCredential.";
        };

        redirectUrl = lib.mkOption {
          type = lib.types.str;
          default = "";
          example = "https://auth.example.com/__cyac/oidc/callback";
          description = "The one redirect URI to register with the provider. Its host doesn't have to be an exposed route.";
        };

        allow = lib.mkOption {
          type = lib.types.listOf lib.types.str;
          default = [ ];
          example = [ "you@example.com" ];
          description = "Subjects or verified emails allowed to sign in. Required: OIDC never opens a route to everyone.";
        };
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

    systemd.services.cyac-serve = lib.mkIf cfg.serve.enable {
      description = "cyac proxy";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment.CYAC_DIR = cfg.stateDir;
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "serve"
          ]
          ++ (
            if cfg.serve.acmeEmail != null then
              [
                "--acme-email"
                cfg.serve.acmeEmail
              ]
            else
              [
                "--listen"
                cfg.serve.listen
              ]
          )
          ++ lib.optionals (oidc.issuer != null) [
            "--oidc-issuer"
            oidc.issuer
            "--oidc-client-id"
            oidc.clientId
            "--oidc-client-secret-file"
            "%d/oidc-client-secret"
            "--oidc-redirect-url"
            oidc.redirectUrl
            "--oidc-allow"
            (lib.concatStringsSep "," oidc.allow)
          ]
        );
        LoadCredential = lib.mkIf (oidc.issuer != null) "oidc-client-secret:${oidc.clientSecretFile}";
        Restart = "on-failure";
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf (
      cfg.serve.enable && cfg.serve.openFirewall && cfg.serve.acmeEmail != null
    ) [ 80 443 ];

    assertions = lib.optionals (cfg.serve.enable && oidc.issuer != null) [
      {
        assertion = oidc.clientSecretFile != null && oidc.allow != [ ];
        message = "services.cyac.serve.oidc needs clientSecretFile and a non-empty allow list.";
      }
    ];

    warnings = lib.optional (config.networking.nftables.enable && config.networking.nftables.flushRuleset) ''
      services.cyac: networking.nftables.flushRuleset wipes cyac's "inet cyac" table on every
      firewall reload, until the next `cyac start` puts it back. Consider flushRuleset = false.
    '';
  };
}
