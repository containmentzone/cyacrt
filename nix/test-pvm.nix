# A VM whose CPU has no VMX/SVM, like a cloud VM without nested virt, boots
# the PVM host kernel; cyac must pick firecracker-pvm and boot a box under
# it. The box is a hand-made busybox disk, since the test has no registry.
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
in
pkgs.testers.runNixOSTest {
  name = "cyac-pvm";
  nodes.host = {
    imports = [ self.nixosModules.default ];
    services.cyac = {
      enable = true;
      kernel = "pvm";
    };
    environment.systemPackages = [ pkgs.e2fsprogs ];
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
    host.succeed("cyac start t")
    host.wait_until_succeeds("grep cyac-pvm-ok /var/lib/cyac/run/t/console.log", timeout=120)
    host.succeed("cyac ls | grep -F 'running (firecracker-pvm)'")
    host.succeed("ping -c1 -W5 10.88.0.2")
    host.succeed("cyac stop t")
  '';
}
