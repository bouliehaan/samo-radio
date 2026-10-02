//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
)

// matchDirOwner gives a freshly written file to whoever owns the directory it
// lives in, when we are root and they are not.
//
// The README's first instruction after installing is `sudo samo-radio
// --pairing`, and pairing mints the control token — so on a fresh box the
// config was first written by root, mode 0600, and the service, which runs as
// samo-radio, could never read its own config again: a restart loop of
// "permission denied" from the very next start. The state directory belongs to
// the service account, so the file it holds should too, whoever wrote it.
func matchDirOwner(path string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid == 0 {
		return nil
	}
	return os.Chown(path, int(stat.Uid), int(stat.Gid))
}
