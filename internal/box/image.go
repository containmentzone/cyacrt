package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/containmentzone/cyacrt/internal/sh"
)

// Images is where images are kept unpacked, once per image ID, for every
// kind of workload to clone its disk from:
//
//	<dataset>/<id>        the image's ext4 filesystem, as a sparse zvol
//	<dataset>/<id>@base   its snapshot, which workload disks are cloned from
//	<dir>/<id>.json       its defaults and when it was last used (Image)
//	<dir>/<id>.lock       held while a workload is seeded from it
//
// <id> is the image ID, the digest of the image's config, without its
// "sha256:". It names one platform's image, never a multi-platform index.
//
// A clone pins its image's snapshot: ZFS won't destroy an image while
// any disk cloned from it exists. Pruning, when there is any, has to
// leave those images be (or `zfs promote` the clone first).
type Images struct {
	Dir     string
	Dataset string
}

// Image is what <dir>/<id>.json holds.
type Image struct {
	Defaults
	// LastUsed is when a workload was last seeded from the image.
	LastUsed time.Time `json:"last_used"`
}

func (i Images) volume(id v1.Hash) string   { return i.Dataset + "/" + id.Hex }
func (i Images) snapshot(id v1.Hash) string { return i.volume(id) + "@base" }
func (i Images) file(id v1.Hash) string     { return filepath.Join(i.Dir, id.Hex+".json") }

// lock holds id's lock until the returned func is called, so two
// workloads seeded from a new image at once unpack it only once.
func (i Images) lock(id v1.Hash) (func(), error) {
	if err := os.MkdirAll(i.Dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(i.Dir, id.Hex+".lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

// get returns id's record, unpacking img first if id has none. The zvol
// and the file come and go together; one without the other is a seed
// that died part way, or a state dir that lost its files, and get leaves
// it be for someone to look at.
func (i Images) get(img v1.Image, id v1.Hash, progress func(string)) (*Image, error) {
	_, volErr := volsize(i.volume(id))
	data, err := os.ReadFile(i.file(id))
	switch {
	case err == nil && volErr == nil:
		var rec Image
		return &rec, json.Unmarshal(data, &rec)
	case err == nil:
		return nil, fmt.Errorf("%s exists but %s doesn't; remove the file to unpack the image again", i.file(id), i.volume(id))
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	case volErr == nil:
		return nil, fmt.Errorf("%s exists but %s doesn't; destroy the zvol (zfs destroy -r) to unpack the image again", i.volume(id), i.file(id))
	}

	progress("unpacking image " + id.String() + " to " + i.volume(id))
	d, err := unpack(img, i.Dir, i.volume(id), 0, progress)
	if err != nil {
		return nil, err
	}
	if err := sh.Run("zfs", "snapshot", i.snapshot(id)); err != nil {
		sh.Run("zfs", "destroy", i.volume(id))
		return nil, err
	}
	return &Image{Defaults: d}, nil
}

// used records that a workload was just seeded from id. Writing the file
// is also what marks a newly unpacked image as complete.
func (i Images) used(id v1.Hash, rec *Image) error {
	rec.LastUsed = time.Now().UTC()
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(i.file(id)+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(i.file(id)+".tmp", i.file(id))
}

// treeSize is how big a disk the unpacked tree at dir needs: its files in
// 4 KiB blocks plus half again for ext4's own metadata and some slack, and
// at least 16 KiB per entry, ext4's default space per inode. It is never
// under 1 GiB, which also keeps mkfs.ext4 off its small-disk settings
// (1 KiB blocks), which a disk grown later would be stuck with.
func treeSize(dir string) (int64, error) {
	var blocks, entries int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entries++
		blocks += (info.Size() + 4095) / 4096
		return nil
	})
	size := max(1<<30, blocks*4096*3/2+256<<20, entries*16<<10*5/4)
	return (size + 1<<20 - 1) &^ (1<<20 - 1), err // whole MiB, as zvols want
}
