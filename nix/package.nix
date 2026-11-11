{
  lib,
  buildGoModule,
  makeWrapper,
  iproute2,
  nftables,
  util-linux,
  e2fsprogs,
  gnutar,
  runc,
  cloud-hypervisor,
  procps,
  wireguard-tools,
}:

let
  # Everything cyac execs. cloud-hypervisor is only used on hosts with KVM.
  runtimeTools = [
    iproute2
    nftables
    util-linux
    e2fsprogs
    gnutar
    runc
    cloud-hypervisor
    procps # sysctl, for `cyac run`
    wireguard-tools # wg, for Kind=wireguard gateways
  ];
in
buildGoModule {
  pname = "cyac";
  version = "0.1.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../go.mod
      ../go.sum
      ../cmd
      ../internal
    ];
  };
  vendorHash = "sha256-I0h4V7tVejZVRYoXHYJfBZtobknvV/8iwtFUN989njU=";
  subPackages = [
    "cmd/cyac"
    "cmd/cyac-generator"
    "cmd/cyac-init"
  ];
  # cyac-init runs in VMs and images, so it must be static.
  env.CGO_ENABLED = 0;

  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    mkdir -p $out/libexec/cyac
    mv $out/bin/cyac-init $out/libexec/cyac/
    wrapProgram $out/bin/cyac --prefix PATH : ${lib.makeBinPath runtimeTools} \
      --set-default CYAC_INIT $out/libexec/cyac/cyac-init
  '';

  passthru = { inherit runtimeTools; };

  meta = {
    description = "Disk-backed boxes under Cloud Hypervisor or runc, behind a closed-by-default proxy";
    mainProgram = "cyac";
    platforms = lib.platforms.linux;
  };
}
