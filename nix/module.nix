# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac = { enable = true; dataset = "rpool/cyac"; };
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
  localCA = cfg.proxy.localCA;
  ownCA = localCA.enable && localCA.certFile != null;
  # For the generated cybox@ and cyservice@ units: added to them, not
  # replacing them.
  workloadDropin = {
    overrideStrategy = "asDropin";
    # The host's zfs, which matches its kernel module, for the disks, which
    # are there once the pools are imported.
    path = [ config.boot.zfs.package ];
    wants = [ "zfs.target" ];
    after = [ "zfs.target" ];
    environment = {
      CYAC_DIR = cfg.stateDir;
      CYAC_ZFS = cfg.dataset;
    }
    // lib.optionalAttrs (cfg.defaultRuntime != null) { CYAC_RUNTIME = cfg.defaultRuntime; };
  };
in
{
  imports = [
    (lib.mkRemovedOptionModule [ "services" "cyac" "kernel" ]
      "Boxes run under Cloud Hypervisor unless they set Runtime= (or services.cyac.defaultRuntime). Set services.cyac.buildGuestKernel = true for a guest kernel."
    )
  ];

  options.services.cyac = {
    enable = lib.mkEnableOption "the cyac runtime (daemon and proxy, CLI, kernel modules, sysctls, state dir)";

    package = lib.mkOption {
      type = lib.types.package;
      default = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
      description = "The cyac package. Its wrapper already carries runc, cloud-hypervisor, iproute2, nftables and friends.";
    };

    stateDir = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/cyac";
      description = "Where box records, run dirs and console logs live (CYAC_DIR).";
    };

    dataset = lib.mkOption {
      type = lib.types.str;
      example = "rpool/cyac";
      description = ''
        ZFS dataset that box and service disks live under, as sparse zvols
        (<dataset>/boxes/<name>, <dataset>/services/<name>; CYAC_ZFS). Its
        pool must exist; cyac creates the datasets below it as needed.
      '';
    };

    defaultRuntime = lib.mkOption {
      type = lib.types.nullOr (
        lib.types.enum [
          "cloud-hypervisor"
          "runc"
          "runsc"
        ]
      );
      default = null;
      example = "runc";
      description = ''
        Runtime for boxes whose file sets no Runtime= (CYAC_RUNTIME). Null
        means cloud-hypervisor. Set "runc" on a machine without KVM whose
        boxes are all trusted: runc is no sandbox. Services ignore this.
      '';
    };

    buildGuestKernel = lib.mkEnableOption ''
      building a guest kernel from the host kernel's source
      (`boot.kernelPackages.kernel`) as the default guestKernel. Without a
      guest kernel, Cloud Hypervisor is unavailable, and only boxes with
      Runtime=runc (or defaultRuntime = "runc") start'';

    guestKernel = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default =
        if cfg.buildGuestKernel then
          pkgs.callPackage ./guest-kernel.nix { inherit (config.boot.kernelPackages.kernel) src version; }
        else
          null;
      defaultText = lib.literalMD ''
        with buildGuestKernel, a guest vmlinux built from the host kernel's
        source (`boot.kernelPackages.kernel`); otherwise none
      '';
      description = ''
        Guest kernel (uncompressed vmlinux) for Cloud Hypervisor, linked to
        stateDir/vmlinux.
      '';
    };

    proxy = {
      domain = lib.mkOption {
        type = lib.types.str;
        example = "boxes.example.com";
        description = ''
          Root domain for exposed boxes: `cyac expose web app=8000`
          serves https://app.web.<domain>. Point a wildcard DNS record
          (*.<domain>) at this host. The proxy always serves :80 and :443 and gets Let's Encrypt
          certificates on demand, using security.acme.defaults.email when
          set; it requires security.acme.acceptTerms, unless localCA is on.
        '';
      };

      localCA = {
        enable = lib.mkEnableOption ''
          certificates from a local CA instead of Let's Encrypt, for hosts
          without internet access. Without certFile and keyFile, the daemon
          makes a CA on first start, limited to the root domain, in
          stateDir/ca. Clients trust it by fetching http://<domain>/ca.crt
          and checking it against the SHA-256 the daemon logs'';

        certFile = lib.mkOption {
          type = lib.types.nullOr lib.types.path;
          default = null;
          example = "/etc/cyac/ca.crt";
          description = "Your own CA certificate (PEM), with keyFile.";
        };

        keyFile = lib.mkOption {
          type = lib.types.nullOr lib.types.str;
          default = null;
          example = "/run/secrets/cyac-ca.key";
          description = ''
            Private key (PEM) of certFile. A path as a string, read at
            runtime through systemd's LoadCredential, so it stays out of
            the nix store.
          '';
        };
      };

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open TCP 80 and 443.";
      };
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ];
    environment.variables.CYAC_DIR = cfg.stateDir;
    environment.variables.CYAC_ZFS = cfg.dataset;

    # Disks are zvols. This needs networking.hostId, as any ZFS host does.
    boot.supportedFilesystems.zfs = true;
    # tun: VM taps; bridge: each network's bridge.
    boot.kernelModules = [
      "tun"
      "bridge"
    ];
    # cyac sets this itself on start; declaring it keeps NixOS from fighting it.
    boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
      # Networking objects: one file per box, service, network or gateway.
      # The path is fixed (cyac-generator runs before any of this config
      # could reach it), whatever stateDir says.
      "d /var/lib/cyac/conf 0700 root root -"
    ]
    ++ lib.optional (cfg.guestKernel != null) "L+ ${cfg.stateDir}/vmlinux - - - - ${cfg.guestKernel}"
    # The header-injecting proxy's image, at a path a cyservice's SeedImage= can
    # name that survives upgrades and garbage collection.
    ++ [
      "d ${cfg.stateDir}/images 0700 root root -"
      "L+ ${cfg.stateDir}/images/cyac-inject.tar - - - - ${
        self.packages.${pkgs.stdenv.hostPlatform.system}.cyac-inject-image
      }"
    ];

    assertions = [
      {
        assertion = localCA.enable || config.security.acme.acceptTerms;
        message = "services.cyac: the proxy uses Let's Encrypt, so it needs security.acme.acceptTerms = true (or proxy.localCA.enable).";
      }
      {
        assertion = (localCA.certFile == null) == (localCA.keyFile == null);
        message = "services.cyac: proxy.localCA.certFile and keyFile go together.";
      }
      {
        assertion = localCA.certFile == null || localCA.enable;
        message = "services.cyac: proxy.localCA.certFile is set, but proxy.localCA.enable is not.";
      }
    ];

    # The daemon owns stateDir and serves the CLI's API on /run/cyac/cyac.sock
    # (root and the cyac group, no other auth for now) and the proxy on the
    # network.
    # Like the docker group: members can drive the daemon, which is as good as
    # root. Add users with users.users.<name>.extraGroups = [ "cyac" ].
    users.groups.cyac = { };
    systemd.services.cyac = {
      description = "cyac daemon";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      environment.CYAC_DIR = cfg.stateDir;
      environment.CYAC_ZFS = cfg.dataset;
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            (lib.getExe cfg.package)
            "daemon"
            "--domain"
            cfg.proxy.domain
            "--group"
            "cyac"
          ]
          ++ lib.optionals (!localCA.enable && config.security.acme.defaults.email != null) [
            "--acme-email"
            config.security.acme.defaults.email
          ]
          ++ lib.optional (localCA.enable && localCA.certFile == null) "--local-ca"
          # %d is the unit's credentials directory.
          ++ lib.optionals ownCA [
            "--ca-cert"
            "%d/ca.crt"
            "--ca-key"
            "%d/ca.key"
          ]
        );
        LoadCredential = lib.mkIf ownCA [
          "ca.crt:${localCA.certFile}"
          "ca.key:${localCA.keyFile}"
        ];
        RuntimeDirectory = "cyac";
        # The socket inside is 0660 root:cyac.
        RuntimeDirectoryMode = "0755";
        Restart = "on-failure";
      };
    };

    # cyac-generator turns the files in /var/lib/cyac/conf into units
    # (cygateway@, cydns@, cynetwork@, cyservice@, cybox@ and cyac.target)
    # at boot and on every `systemctl daemon-reload`. These drop-ins give
    # the workload units the daemon's state dir and ZFS dataset.
    systemd.generators.cyac = "${cfg.package}/bin/cyac-generator";
    systemd.services."cybox@" = workloadDropin;
    systemd.services."cyservice@" = workloadDropin;

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.proxy.openFirewall [
      80
      443
    ];

    warnings =
      lib.optional (config.networking.nftables.enable && config.networking.nftables.flushRuleset)
        ''
          services.cyac: networking.nftables.flushRuleset wipes the nft tables cyac's gateway, network
          and box units load, on every firewall reload, until those units restart. Consider flushRuleset = false.
        '';
  };
}
