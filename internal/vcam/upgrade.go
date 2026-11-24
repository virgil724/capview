package vcam

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	oldDLLPrefix = "capview_vcam.dll."
	oldDLLSuffix = ".old"
)

// FileSHA256 returns the lowercase hex SHA-256 digest of the file at path.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CleanupOldDLLs best-effort deletes leftover capview_vcam.dll.*.old files in dir.
// Locked files (still held by FrameServer) and missing directories are silently ignored.
func CleanupOldDLLs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, oldDLLPrefix) && strings.HasSuffix(name, oldDLLSuffix) {
			if err := os.Remove(filepath.Join(dir, name)); err == nil {
				removed++
			}
		}
	}
	return removed
}

type deployOps interface {
	exists(path string) (bool, error)
	sha256(path string) (string, error)
	copyFile(src, dst string) error
	renameFile(oldPath, newPath string) error
	setOldACL(path string) error
	scheduleDeleteOnReboot(path string) error
	stopFrameServer() error
	startFrameServer() error
	removeFile(path string) error
	now() time.Time
}

func deployDLL(src, dst string, mode UpgradeMode, ops deployOps) (UpgradeResult, error) {
	if mode == "" {
		mode = UpgradeAuto
	}
	newHash, err := ops.sha256(src)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("hash bundled DLL %s: %w", src, err)
	}

	dstExists, err := ops.exists(dst)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("stat installed DLL %s: %w", dst, err)
	}
	if !dstExists {
		if err := ops.copyFile(src, dst); err != nil {
			return UpgradeResult{}, fmt.Errorf("copy DLL to %s: %w", dst, err)
		}
		return UpgradeResult{
			Tier:      "fresh",
			NewSHA256: newHash,
		}, nil
	}

	oldHash, err := ops.sha256(dst)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("hash installed DLL %s: %w", dst, err)
	}
	if oldHash == newHash && mode == UpgradeAuto {
		return UpgradeResult{
			Tier:      "noop",
			OldSHA256: oldHash,
			NewSHA256: newHash,
		}, nil
	}

	if mode == UpgradeAuto || mode == UpgradeRename {
		ts := ops.now().UTC().Format("20060102T150405Z")
		backup := fmt.Sprintf("%s.%s%s", dst, ts, oldDLLSuffix)
		if err := ops.renameFile(dst, backup); err == nil {
			_ = ops.setOldACL(backup)
			if err := ops.copyFile(src, dst); err != nil {
				_ = ops.renameFile(backup, dst)
				return UpgradeResult{}, fmt.Errorf("copy upgraded DLL after rename: %w", err)
			}
			_ = ops.scheduleDeleteOnReboot(backup)
			return UpgradeResult{
				Tier:         "rename",
				OldBackup:    backup,
				OldSHA256:    oldHash,
				NewSHA256:    newHash,
				PendingClean: true,
			}, nil
		} else if mode == UpgradeRename {
			return UpgradeResult{}, fmt.Errorf("tier-1 rename %s -> %s: %w", dst, backup, err)
		}
	}

	// Tier 2 fallback (or forced UpgradeRestart): stop FrameServer, replace DLL, restart FrameServer.
	if err := ops.stopFrameServer(); err != nil {
		return UpgradeResult{}, fmt.Errorf("tier-2 stop FrameServer: %w", err)
	}

	var copyErr error
	for attempt := 0; attempt < 5; attempt++ {
		copyErr = ops.copyFile(src, dst)
		if copyErr == nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	startErr := ops.startFrameServer()
	if copyErr != nil {
		return UpgradeResult{}, fmt.Errorf("tier-2 copy DLL to %s: %w (restart err: %v)", dst, copyErr, startErr)
	}
	if startErr != nil {
		return UpgradeResult{}, fmt.Errorf("tier-2 start FrameServer: %w", startErr)
	}

	return UpgradeResult{
		Tier:      "frameserver-restart",
		OldSHA256: oldHash,
		NewSHA256: newHash,
	}, nil
}

func removeInstalledDLL(dst string, mode UpgradeMode, ops deployOps) (string, error) {
	if mode == "" {
		mode = UpgradeAuto
	}
	exists, err := ops.exists(dst)
	if err != nil {
		return "", err
	}
	if !exists {
		return "absent", nil
	}

	if mode == UpgradeRestart {
		if err := ops.stopFrameServer(); err != nil {
			return "", fmt.Errorf("stop FrameServer for uninstall: %w", err)
		}
		rmErr := ops.removeFile(dst)
		startErr := ops.startFrameServer()
		if rmErr != nil {
			return "", fmt.Errorf("remove DLL after stopping FrameServer: %w (restart err: %v)", rmErr, startErr)
		}
		if startErr != nil {
			return "", fmt.Errorf("restart FrameServer after uninstall: %w", startErr)
		}
		return "frameserver-restart", nil
	}

	if err := ops.removeFile(dst); err == nil {
		return "removed", nil
	}

	if mode == UpgradeAuto || mode == UpgradeRename {
		ts := ops.now().UTC().Format("20060102T150405Z")
		backup := fmt.Sprintf("%s.%s%s", dst, ts, oldDLLSuffix)
		if err := ops.renameFile(dst, backup); err == nil {
			_ = ops.setOldACL(backup)
			_ = ops.scheduleDeleteOnReboot(backup)
			return "rename-deferred", nil
		} else if mode == UpgradeRename {
			return "", fmt.Errorf("remove/rename locked DLL %s: %w", dst, err)
		}
	}

	if err := ops.stopFrameServer(); err != nil {
		return "", fmt.Errorf("stop FrameServer for uninstall: %w", err)
	}
	rmErr := ops.removeFile(dst)
	startErr := ops.startFrameServer()
	if rmErr != nil {
		return "", fmt.Errorf("remove DLL after stopping FrameServer: %w (restart err: %v)", rmErr, startErr)
	}
	if startErr != nil {
		return "", fmt.Errorf("restart FrameServer after uninstall: %w", startErr)
	}
	return "frameserver-restart", nil
}

func copyFileAtomic(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}
