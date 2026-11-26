# cyac-tailnet as an image a cyservice can use: one endpoint on a tailnet,
# forwarded to the service's own port. The service runs its own tailscaled
# in userspace mode (no TUN, nothing on the host's tailnet) with in-memory
# state, logs in with an auth key from its secrets, and hands every
# connection to `tailscale nc`. No Go of its own: tailscale and socat come
# from nixpkgs.
{
  writeShellApplication,
  dockerTools,
  cacert,
  tailscale,
  socat,
  version,
}:

let
  bin = writeShellApplication {
    name = "cyac-tailnet";
    runtimeInputs = [
      tailscale
      socat
    ];
    text = ''
      if [ $# -ne 2 ]; then
        echo "usage: cyac-tailnet <listen-port> <tailnet-host>:<port>" >&2
        exit 2
      fi
      listen=$1 target=$2
      key=''${TS_AUTHKEY_FILE:-/run/secrets/authkey}
      sock=/run/tailscaled.sock

      # /run and /tmp are tmpfs in the container; nothing outlives a run.
      tailscaled --tun=userspace-networking --state=mem: --statedir=/tmp/tailscale --socket="$sock" &
      tailscale --socket="$sock" up --timeout=2m --auth-key="file:$key" ''${TS_HOSTNAME:+--hostname="$TS_HOSTNAME"}

      echo "forwarding :$listen to $target on the tailnet"
      socat "TCP-LISTEN:$listen,fork,reuseaddr" "EXEC:tailscale --socket=$sock nc ''${target%:*} ''${target##*:}" &
      # Whichever of the two dies first takes the service down with it.
      wait -n
      exit 1
    '';
  };
in
dockerTools.buildLayeredImage {
  name = "cyac-tailnet";
  tag = version;
  # cyac reads the tarball as is; it doesn't gunzip it.
  compressor = "none";
  # Links /bin/cyac-tailnet, so a service's Exec= can name it.
  contents = [ bin ];
  config = {
    Entrypoint = [ "/bin/cyac-tailnet" ];
    Env = [ "SSL_CERT_FILE=${cacert}/etc/ssl/certs/ca-bundle.crt" ];
  };
}
