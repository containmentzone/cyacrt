# A VM whose CPU has no VMX/SVM, like a cloud VM without nested virt, boots
# the PVM host kernel; cyac must pick cloud-hypervisor-pvm and boot boxes under
# it, through cyac-init: a busybox disk in init and command mode, and a
# NixOS disk. The disks are hand-made, since the test has no registry.
# Run with `nix build .#checks.x86_64-linux.pvm -L` on a host with KVM.
{ pkgs, self }:

let
  rootfs = pkgs.runCommand "pvm-test-rootfs" { } ''
    mkdir -p $out/bin $out/proc $out/dev
    cp ${pkgs.pkgsStatic.busybox}/bin/busybox $out/bin/
    ln -s busybox $out/bin/sh
    cat > $out/init <<'EOS'
    #!/bin/sh
    echo "cyac-pvm-ok $(busybox uname -r)"
    exec busybox sleep 100000
    EOS
    chmod +x $out/init
  '';

  # A NixOS root disk: no kernel, initrd or boot loader, since cyac brings
  # the kernel and cyac-init stands in for stage 1.
  nixos =
    (pkgs.nixos {
      boot.loader.grub.enable = false;
      boot.kernel.enable = false;
      boot.initrd.enable = false;
      fileSystems."/" = {
        device = "/dev/vda";
        fsType = "ext4";
      };
      networking.useDHCP = false; # the kernel's ip= argument sets up eth0
      documentation.enable = false;
      nix.enable = false;
      system.stateVersion = pkgs.lib.trivial.release;
    }).config.system.build.toplevel;
  nixosDisk = pkgs.callPackage "${pkgs.path}/nixos/lib/make-ext4-fs.nix" {
    storePaths = [ nixos ];
    volumeLabel = "nixos";
    populateImageCommands = ''
      mkdir -p ./files/sbin
      ln -s ${nixos}/init ./files/sbin/init
    '';
  };
in
pkgs.testers.runNixOSTest {
  name = "cyac-pvm";
  nodes.host = {
    imports = [ self.nixosModules.default ];
    services.cyac = {
      enable = true;
      kernel = "pvm";
      proxy.domain = "boxes.test";
    };
    security.acme.acceptTerms = true;
    # The module's PVM kernel with only the drivers this VM and cyac use.
    boot.kernelPackages = pkgs.lib.mkForce (
      pkgs.linuxPackagesFor
        (pkgs.callPackage ./pvm.nix {
          autoModules = false;
          extraConfig = with pkgs.lib.kernel; {
            VIRTIO_PCI = yes;
            VIRTIO_NET = yes;
            VIRTIO_BLK = yes;
            VIRTIO_CONSOLE = yes;
            VIRTIO_BALLOON = yes;
            HW_RANDOM_VIRTIO = yes;
            SCSI_VIRTIO = yes;
            NET_9P = yes;
            NET_9P_VIRTIO = yes;
            "9P_FS" = yes;
            "9P_FS_POSIX_ACL" = yes;
            # The test VM's initrd mounts its writable /nix/store as an overlay.
            OVERLAY_FS = yes;
            # What cyac needs: kvm-pvm, tap devices on a bridge, and the
            # nftables NAT in internal/network/bridge.
            VIRTUALIZATION = yes;
            KVM = module;
            TUN = yes;
            BRIDGE = yes;
            NETFILTER = yes;
            NF_CONNTRACK = yes;
            NF_NAT = yes;
            NF_TABLES = yes;
            NF_TABLES_INET = yes;
            NFT_CT = yes;
            NFT_NAT = yes;
            NFT_MASQ = yes;
          };
        }).hostKernel
    );
    boot.initrd.includeDefaultModules = false;
    # The test VM's own module lists, less what this kernel leaves out:
    # virtio_mmio, virtiofs and virtio_gpu, which a QEMU PCI VM doesn't need.
    boot.initrd.availableKernelModules = pkgs.lib.mkForce [
      "9p"
      "9pnet_virtio"
      "autofs"
      "efivarfs"
      "ext4"
      "overlay"
      "tpm-crb"
      "tpm-tis"
      "virtio_blk"
      "virtio_net"
      "virtio_pci"
      "virtio_scsi"
    ];
    boot.initrd.kernelModules = pkgs.lib.mkForce [
      "virtio_balloon"
      "virtio_console"
      "virtio_rng"
    ];
    # The NixOS firewall wants iptables modules this kernel leaves out.
    networking.firewall.enable = false;
    environment.systemPackages = [
      pkgs.e2fsprogs
      pkgs.curl
    ];
    virtualisation = {
      memorySize = 4096;
      cores = 2;
      qemu.options = [ "-cpu host,-vmx,-svm" ];
    };
  };

  testScript = ''
    host.wait_for_unit("cyac.service")
    host.fail("grep -qwE 'vmx|svm' /proc/cpuinfo")
    host.succeed("test -c /dev/kvm && test -d /sys/module/kvm_pvm")

    host.succeed(
        "mkdir -p /var/lib/cyac/boxes /var/lib/cyac/disks",
        "truncate -s 256M /var/lib/cyac/disks/t.ext4",
        "mkfs.ext4 -q -d ${rootfs} /var/lib/cyac/disks/t.ext4",
        """echo '{"name":"t","image":"test","id":2,"cpus":1,"mem_mib":256,"init":["/init"]}' > /var/lib/cyac/boxes/t.json""",
    )
    # The proxy always listens on :80, which only redirects to HTTPS.
    host.succeed("curl -sI http://t.boxes.test/ --resolve t.boxes.test:80:127.0.0.1 | grep -i '^location: https://t.boxes.test/'")

    host.succeed("cyac start t")
    host.wait_until_succeeds("grep cyac-pvm-ok /var/lib/cyac/logs/t.console.log", timeout=120)
    host.succeed("cyac ls | grep -F 'running (cloud-hypervisor-pvm)'")
    host.succeed("ping -c1 -W5 10.42.0.2")
    host.wait_until_succeeds("cyac shell t busybox echo agent-ok | grep agent-ok", timeout=60)
    host.succeed("cyac stop t")

    # Command mode: the box stops, rather than restarts, when its command exits.
    host.succeed(
        "cp /var/lib/cyac/disks/t.ext4 /var/lib/cyac/disks/c.ext4",
        """echo '{"name":"c","image":"test","id":3,"cpus":1,"mem_mib":256,"init":["/bin/sh","-c","echo cmd-ok; exit 3"],"mode":"command"}' > /var/lib/cyac/boxes/c.json""",
        "cyac start c",
    )
    host.wait_until_succeeds("cyac ls | grep -E '^c .* stopped'", timeout=120)
    host.succeed(
        "grep cmd-ok /var/lib/cyac/logs/c.console.log",
        "grep -F 'cyac-init: powering off: /bin/sh exited (status 3)' /var/lib/cyac/logs/c.console.log",
        "journalctl -u cyac | grep -F 'box exited on its own; marking it stopped' | grep -F box=c",
    )

    # NixOS boots with cyac-init as its stage 1.
    host.succeed(
        "cp ${nixosDisk} /var/lib/cyac/disks/n.ext4 && chmod 600 /var/lib/cyac/disks/n.ext4",
        "truncate -s 2G /var/lib/cyac/disks/n.ext4 && e2fsck -fy /var/lib/cyac/disks/n.ext4; resize2fs /var/lib/cyac/disks/n.ext4",
        """echo '{"name":"n","image":"nixos","id":4,"cpus":1,"mem_mib":1024,"init":["/sbin/init"]}' > /var/lib/cyac/boxes/n.json""",
        "cyac start n",
    )
    host.wait_until_succeeds("cyac shell n systemctl is-system-running | grep -E 'running|degraded'", timeout=300)
    host.succeed("cyac shell n cat /etc/os-release | grep -F 'ID=nixos'")

    # A guest reboot resets the VM in place: the box stays up.
    host.execute("cyac shell n systemctl reboot")
    host.wait_until_succeeds("grep -F 'reboot: Restarting system' /var/lib/cyac/logs/n.console.log", timeout=120)
    host.wait_until_succeeds("cyac shell n systemctl is-system-running | grep -E 'running|degraded'", timeout=300)
    host.succeed("cyac ls | grep -E '^n .*running'")

    # A stop is a clean poweroff, asked for through the agent.
    host.succeed("cyac stop n")
    host.succeed("grep -F 'reboot: Power down' /var/lib/cyac/logs/n.console.log")
  '';
}
