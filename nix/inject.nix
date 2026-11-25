# cyac-inject (cmd/cyac-inject) as an image a cyservice can use: an
# uncompressed `docker save` tarball holding the static binary and a CA
# bundle, nothing else. Point a service's SeedImage= at the store path.
{
  buildGoModule,
  dockerTools,
  cacert,
  cyac,
}:

let
  bin = buildGoModule {
    pname = "cyac-inject";
    inherit (cyac) version src vendorHash;
    subPackages = [ "cmd/cyac-inject" ];
    env.CGO_ENABLED = 0;
  };
in
dockerTools.buildLayeredImage {
  name = "cyac-inject";
  tag = bin.version;
  # cyac reads the tarball as is; it doesn't gunzip it.
  compressor = "none";
  # Links /bin/cyac-inject, so a service's Exec= can name it.
  contents = [ bin ];
  config = {
    Entrypoint = [ "/bin/cyac-inject" ];
    Env = [ "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt" ];
  };
}
