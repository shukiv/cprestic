package bugreport

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
)

const IntakeKeyFile = "bugs-intake.key"

// ReadIntakeKey reads a per-installation secret, never one embedded in the
// public plugin. It must be a private regular file owned by the service uid
// (root on cPanel). O_NONBLOCK keeps a malicious FIFO from hanging a UI page.
func ReadIntakeKey(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("install a private intake key at %s to enable sending", path)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("cannot verify intake key permissions")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", fmt.Errorf("the intake key must be a private regular file owned by the service user (root on cPanel), mode 0600")
	}
	body, err := io.ReadAll(io.LimitReader(f, 4098))
	if err != nil || len(body) > 4097 {
		return "", fmt.Errorf("cannot read intake key or key file is too large")
	}
	key := strings.TrimSpace(string(body))
	if err := usableIntakeKey(key); err != nil {
		return "", err
	}
	return key, nil
}
