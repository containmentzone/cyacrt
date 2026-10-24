# Host setup for cyac on NixOS. Import via the flake:
#
#   imports = [ cyacrt.nixosModules.default ];
#   services.cyac = { enable = true; kernel = "none"; };
self:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  cfg = config.services.cyac;
  oidc = cfg.proxy.oidc;
  pvm = cfg.kernel == "pvm";
  vmlinux = if pvm then "vmlinux-pvm" else "vmlinux";
in
{
  imports = [
    (lib.mkRemovedOptionModule [ "services" "cyac" "proxy" "listen" ]
      "The proxy always serves :80 and :443 now; set services.cyac.proxy.domain."
    )
    (lib.mkRemovedOptionModule [ "services" "cyac" "proxy" "acme" ]
      "The proxy always uses Let's Encrypt now; set services.cyac.proxy.domain."
    )
  ];

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
      type = lib.types.nullOr (
        lib.types.enum [
          "kvm"
          "pvm"
          "none"
        ]
      );
      default = null;
      description = ''
        How the host kernel provides virtualization. Must be set.

        - kvm: hardware KVM (kvm-intel/kvm-amd, as your hardware config
          loads them). Pins cyac to the firecracker runtime. The guest
          kernel is built from the host kernel's source by default.
        - pvm: no hardware virtualization needed, e.g. a cloud VM without
          nested virt. Boots the host into virt-pvm's kernel with pti=off,
          loads kvm-pvm instead of kvm-intel/kvm-amd, and pins cyac to the
          firecracker-pvm runtime. x86_64 only.
        - none: leaves the kernel alone and lets cyac pick a runtime itself
          (firecracker, firecracker-pvm, then runc).
      '';
    };

    guestKernel = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default =
        if pvm then
          self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-guest-kernel
        else if cfg.kernel == "kvm" then
          pkgs.callPackage ./guest-kernel.nix { inherit (config.boot.kernelPackages.kernel) src version; }
        else
          null;
      defaultText = lib.literalMD ''
        for "kvm", a guest vmlinux built from the host kernel's source
        (`boot.kernelPackages.kernel`); for "pvm", the PVM guest kernel
      '';
      description = ''
        Guest kernel (uncompressed vmlinux) for Firecracker, linked to
        stateDir/vmlinux, or stateDir/vmlinux-pvm with kernel = "pvm".
      '';
    };

    proxy = {
      domain = lib.mkOption {
        type = lib.types.str;
        example = "boxes.example.com";
        description = ''
          Root domain for exposed boxes: `cyac expose web 8000` serves
          https://web.<domain>. Point a wildcard DNS record for it at this
          host. The proxy always serves :80 and :443 and gets Let's Encrypt
          certificates on demand, using security.acme.defaults.email when
          set; it requires security.acme.acceptTerms.
        '';
      };

      openFirewall = lib.mkOption {
        type = lib.types.bool;
        default = false;
        description = "Open TCP 80 and 443.";
      };

      oidc = lib.mkOption {
        default = null;
        description = ''
          Optional OIDC sign-in for non-public routes. Allowed users sign in
          with the provider; route tokens keep working alongside it.
        '';
        type = lib.types.nullOr (
          lib.types.submodule {
            options = {
              issuer = lib.mkOption {
                type = lib.types.str;
                example = "https://id.example.com";
                description = "OIDC issuer URL.";
              };
              clientId = lib.mkOption {
                type = lib.types.str;
                description = "OIDC client ID.";
              };
              clientSecretFile = lib.mkOption {
                # A string, not a path, so the secret is never copied into the store.
                type = lib.types.str;
                example = "/run/secrets/cyac-oidc";
                description = ''
                  Absolute path to a file holding the client secret, e.g. from
                  agenix or sops-nix. systemd reads it at service start
                  (LoadCredential); it never enters the Nix store.
                '';
              };
              redirectUrl = lib.mkOption {
                type = lib.types.str;
                example = "https://auth.example.com/__cyac/oidc/callback";
                description = ''
                  The one redirect URI to register with the provider. Its host
                  doesn't have to be a box; pick a name no box will take.
                '';
              };
              allow = lib.mkOption {
                type = lib.types.nonEmptyListOf lib.types.str;
                example = [ "you@example.com" ];
                description = "Subjects or verified emails allowed to sign in.";
              };
            };
          }
        );
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
    ]
    ++ lib.optional pvm "kvm-pvm";
    # cyac sets this itself on start; declaring it keeps NixOS from fighting it.
    boot.kernel.sysctl."net.ipv4.ip_forward" = lib.mkDefault 1;

    systemd.tmpfiles.rules = [
      "d ${cfg.stateDir} 0700 root root -"
    ]
    ++ lib.optional (cfg.guestKernel != null) "L+ ${cfg.stateDir}/${vmlinux} - - - - ${cfg.guestKernel}";

    # PVM: virt-pvm's host kernel, without page table isolation (PVM doesn't
    # support it), and kvm-pvm, which can't be loaded alongside kvm-intel or
    # kvm-amd.
    boot.kernelPackages = lib.mkIf pvm (
      pkgs.linuxPackagesFor self.packages.${pkgs.stdenv.hostPlatform.system}.pvm-host-kernel
    );
    boot.kernelParams = lib.mkIf pvm [ "pti=off" ];
    boot.blacklistedKernelModules = lib.mkIf pvm [
      "kvm-intel"
      "kvm-amd"
    ];
    assertions = [
      {
        assertion = cfg.kernel != null;
        message = ''services.cyac.kernel must be set: "kvm", "pvm" or "none".'';
      }
      {
        assertion = !pvm || pkgs.stdenv.hostPlatform.isx86_64;
        message = "services.cyac: kernel = \"pvm\" is x86_64-only.";
      }
      {
        assertion = cfg.kernel != "kvm" || cfg.guestKernel != null;
        message = ''services.cyac: kernel = "kvm" needs a guestKernel.'';
      }
    ]
    ++ [
      {
        assertion = oidc == null || (lib.hasPrefix "/" oidc.clientSecretFile && !lib.hasPrefix builtins.storeDir oidc.clientSecretFile);
        message = "services.cyac.proxy.oidc.clientSecretFile must be an absolute path outside the Nix store.";
      }
      {
        assertion = config.security.acme.acceptTerms;
        message = "services.cyac: the proxy uses Let's Encrypt, so it needs security.acme.acceptTerms = true.";
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
      environment.CYAC_RUNTIME = lib.mkIf (cfg.kernel == "kvm" || pvm) (
        if pvm then "firecracker-pvm" else "firecracker"
      );
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
          ++ lib.optionals (config.security.acme.defaults.email != null) [
            "--acme-email"
            config.security.acme.defaults.email
          ]
          ++ lib.optionals (oidc != null) [
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
        LoadCredential = lib.mkIf (oidc != null) "oidc-client-secret:${oidc.clientSecretFile}";
        RuntimeDirectory = "cyac";
        # The socket inside is 0660 root:cyac.
        RuntimeDirectoryMode = "0755";
        # The daemon stops its boxes on SIGTERM (each gets up to 30s for a
        # clean shutdown, all at once); whatever is left after that is killed.
        KillMode = "mixed";
        Restart = "on-failure";
      };
    };

    networking.firewall.allowedTCPPorts = lib.mkIf cfg.proxy.openFirewall [
      80
      443
    ];

    warnings = lib.optional (config.networking.nftables.enable && config.networking.nftables.flushRuleset) ''
      services.cyac: networking.nftables.flushRuleset wipes cyac's "inet cyac" table on every
      firewall reload, until the next `cyac start` puts it back. Consider flushRuleset = false.
    '';
  };
}
