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
  firecracker,
  openssh,
}:

let
  # Everything cyac execs. firecracker is only used on hosts with KVM, and
  # ssh only to exec into its VMs.
  runtimeTools = [
    iproute2
    nftables
    util-linux
    e2fsprogs
    gnutar
    runc
    firecracker
    openssh
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
  vendorHash = "sha256-FWHDt16ywvV53UaOSc6HGylsGVl4jikH2eKgl82xRAE=";
  subPackages = [ "cmd/cyac" ];

  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/cyac --prefix PATH : ${lib.makeBinPath runtimeTools}
  '';

  passthru = { inherit runtimeTools; };

  meta = {
    description = "Disk-backed boxes under Firecracker or runc, behind a closed-by-default proxy";
    mainProgram = "cyac";
    platforms = lib.platforms.linux;
  };
}
