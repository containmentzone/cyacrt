package box

import (
	"crypto/ed25519"
	"encoding/pem"
	"errors"
	"io/fs"
	"os"
	"path"

	"golang.org/x/crypto/ssh"

	"github.com/containmentzone/cyacrt/internal/driver"
)

// seedSSH lets the daemon reach the box's sshd, which is how it runs
// commands in VMs. It generates two keys: one the daemon logs in with
// (written to authorized_keys for root), and the box's host key, which the
// daemon pins so a box that takes over another box's IP can't pose as it.
// It also undoes masking of the image's ssh units (exeuntu masks them) and
// enables ssh.service if nothing starts sshd. Images without sshd just
// can't be exec'd into as VMs.
func seedSSH(root *os.Root, m *driver.Machine) error {
	client, err := newKey()
	if err != nil {
		return err
	}
	host, err := newKey()
	if err != nil {
		return err
	}
	if err := os.WriteFile(m.SSHKey, client.private, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(m.KnownHosts, []byte(m.Addr.IP.String()+" "+string(host.public)), 0o600); err != nil {
		return err
	}
	for dir, mode := range map[string]os.FileMode{"root": 0o700, "root/.ssh": 0o700, "etc/ssh": 0o755} {
		if err := root.MkdirAll(dir, mode); err != nil {
			return err
		}
	}
	files := []struct {
		name    string
		content []byte
		mode    os.FileMode
	}{
		{"root/.ssh/authorized_keys", client.public, 0o600},
		{"etc/ssh/ssh_host_ed25519_key", host.private, 0o600},
		{"etc/ssh/ssh_host_ed25519_key.pub", host.public, 0o644},
	}
	for _, f := range files {
		if err := writeFile(root, f.name, f.content, f.mode); err != nil {
			return err
		}
	}

	const units = "etc/systemd/system"
	for _, unit := range []string{"ssh.service", "ssh.socket", "sshd.service"} {
		if target, err := root.Readlink(path.Join(units, unit)); err == nil && target == "/dev/null" {
			if err := root.Remove(path.Join(units, unit)); err != nil {
				return err
			}
		}
	}
	for _, wanted := range []string{"multi-user.target.wants/ssh.service", "multi-user.target.wants/sshd.service", "sockets.target.wants/ssh.socket"} {
		if _, err := root.Lstat(path.Join(units, wanted)); err == nil {
			return nil // something already starts sshd
		}
	}
	const unit = "/usr/lib/systemd/system/ssh.service"
	if _, err := root.Stat(unit[1:]); errors.Is(err, fs.ErrNotExist) {
		return nil // no sshd in this image
	}
	if err := root.MkdirAll(path.Join(units, "multi-user.target.wants"), 0o755); err != nil {
		return err
	}
	return root.Symlink(unit, path.Join(units, "multi-user.target.wants/ssh.service"))
}

type keyPair struct{ private, public []byte }

func newKey() (keyPair, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return keyPair{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, "cyac")
	if err != nil {
		return keyPair{}, err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return keyPair{}, err
	}
	return keyPair{private: pem.EncodeToMemory(block), public: ssh.MarshalAuthorizedKey(sshPub)}, nil
}
