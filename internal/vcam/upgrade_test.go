package vcam

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"
)

func TestShmHeaderLayout(t *testing.T) {
	var h ShmHeader
	if got := unsafe.Sizeof(ShmSlotMeta{}); got != 32 {
		t.Fatalf("ShmSlotMeta size = %d, want 32", got)
	}
	if got := unsafe.Sizeof(h); got != 168 {
		t.Fatalf("ShmHeader size = %d, want 168", got)
	}
	checks := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"Magic", unsafe.Offsetof(h.Magic), 0},
		{"Version", unsafe.Offsetof(h.Version), 4},
		{"SlotCount", unsafe.Offsetof(h.SlotCount), 8},
		{"SlotStride", unsafe.Offsetof(h.SlotStride), 12},
		{"DataOffset", unsafe.Offsetof(h.DataOffset), 16},
		{"ProducerPID", unsafe.Offsetof(h.ProducerPID), 20},
		{"Active", unsafe.Offsetof(h.Active), 24},
		{"Pad0", unsafe.Offsetof(h.Pad0), 28},
		{"Seq", unsafe.Offsetof(h.Seq), 32},
		{"LatestSlot", unsafe.Offsetof(h.LatestSlot), 40},
		{"Width", unsafe.Offsetof(h.Width), 44},
		{"Height", unsafe.Offsetof(h.Height), 48},
		{"FPSNum", unsafe.Offsetof(h.FPSNum), 52},
		{"FPSDen", unsafe.Offsetof(h.FPSDen), 56},
		{"Format", unsafe.Offsetof(h.Format), 60},
		{"TimestampHNS", unsafe.Offsetof(h.TimestampHNS), 64},
		{"Slots", unsafe.Offsetof(h.Slots), 72},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("offsetof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
	if got := SlotOffset(2); got != 4096+2*12*1024*1024 {
		t.Fatalf("SlotOffset(2) = %d", got)
	}
}

func TestParseUpgradeMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    UpgradeMode
		wantErr bool
	}{
		{"", UpgradeAuto, false},
		{"auto", UpgradeAuto, false},
		{"RENAME", UpgradeRename, false},
		{"restart", UpgradeRestart, false},
		{"frameserver-restart", UpgradeRestart, false},
		{"bogus", "", true},
	} {
		got, err := ParseUpgradeMode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("ParseUpgradeMode(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseUpgradeMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFileSHA256AndCleanupOldDLLs(t *testing.T) {
	dir := t.TempDir()
	keep := filepath.Join(dir, "capview_vcam.dll")
	old1 := filepath.Join(dir, "capview_vcam.dll.20261124T010203Z.old")
	old2 := filepath.Join(dir, "capview_vcam.dll.123.old")
	other := filepath.Join(dir, "unrelated.old")
	for _, p := range []string{keep, old1, old2, other} {
		if err := os.WriteFile(p, []byte("payload-"+filepath.Base(p)), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	gotHash, err := FileSHA256(keep)
	if err != nil {
		t.Fatalf("FileSHA256: %v", err)
	}
	sum := sha256.Sum256([]byte("payload-capview_vcam.dll"))
	if want := hex.EncodeToString(sum[:]); gotHash != want {
		t.Fatalf("FileSHA256 = %q, want %q", gotHash, want)
	}

	n := CleanupOldDLLs(dir)
	if n != 2 {
		t.Fatalf("CleanupOldDLLs removed %d files, want 2", n)
	}
	for _, p := range []string{keep, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("expected %s to remain: %v", p, err)
		}
	}
	for _, p := range []string{old1, old2} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("expected %s to be removed, stat err=%v", p, err)
		}
	}
	if CleanupOldDLLs(filepath.Join(dir, "missing")) != 0 {
		t.Fatal("expected 0 for missing dir")
	}
}

type fakeDeployOps struct {
	files        map[string][]byte
	renameErr    error
	copyErr      error
	removeErr    error
	stopErr      error
	startErr     error
	stoppedCount int
	startedCount int
	rebootDel    []string
	aclSet       []string
	fixedNow     time.Time
}

func (f *fakeDeployOps) exists(p string) (bool, error) {
	_, ok := f.files[p]
	return ok, nil
}

func (f *fakeDeployOps) sha256(p string) (string, error) {
	b, ok := f.files[p]
	if !ok {
		return "", os.ErrNotExist
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func (f *fakeDeployOps) copyFile(src, dst string) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	b, ok := f.files[src]
	if !ok {
		return os.ErrNotExist
	}
	f.files[dst] = append([]byte(nil), b...)
	return nil
}

func (f *fakeDeployOps) renameFile(oldPath, newPath string) error {
	if f.renameErr != nil {
		return f.renameErr
	}
	b, ok := f.files[oldPath]
	if !ok {
		return os.ErrNotExist
	}
	f.files[newPath] = b
	delete(f.files, oldPath)
	return nil
}

func (f *fakeDeployOps) setOldACL(p string) error {
	f.aclSet = append(f.aclSet, p)
	return nil
}

func (f *fakeDeployOps) scheduleDeleteOnReboot(p string) error {
	f.rebootDel = append(f.rebootDel, p)
	return nil
}

func (f *fakeDeployOps) stopFrameServer() error {
	f.stoppedCount++
	return f.stopErr
}

func (f *fakeDeployOps) startFrameServer() error {
	f.startedCount++
	return f.startErr
}

func (f *fakeDeployOps) removeFile(p string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	if _, ok := f.files[p]; !ok {
		return os.ErrNotExist
	}
	delete(f.files, p)
	return nil
}

func (f *fakeDeployOps) now() time.Time { return f.fixedNow }

func TestDeployDLLModesAndFallbacks(t *testing.T) {
	src := `C:\src\capview_vcam.dll`
	dst := InstalledDLLPath
	fixed := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		ops := &fakeDeployOps{files: map[string][]byte{src: []byte("v1")}, fixedNow: fixed}
		res, err := deployDLL(src, dst, UpgradeAuto, BackendMF, ops)
		if err != nil || res.Tier != "fresh" || string(ops.files[dst]) != "v1" {
			t.Fatalf("fresh failed: res=%+v err=%v", res, err)
		}
	})

	t.Run("noop_same_hash", func(t *testing.T) {
		ops := &fakeDeployOps{files: map[string][]byte{src: []byte("v1"), dst: []byte("v1")}, fixedNow: fixed}
		res, err := deployDLL(src, dst, UpgradeAuto, BackendMF, ops)
		if err != nil || res.Tier != "noop" || ops.stoppedCount != 0 {
			t.Fatalf("noop failed: res=%+v err=%v stops=%d", res, err, ops.stoppedCount)
		}
	})

	t.Run("tier1_rename", func(t *testing.T) {
		ops := &fakeDeployOps{files: map[string][]byte{src: []byte("v2"), dst: []byte("v1")}, fixedNow: fixed}
		res, err := deployDLL(src, dst, UpgradeAuto, BackendMF, ops)
		if err != nil || res.Tier != "rename" {
			t.Fatalf("tier1 rename failed: res=%+v err=%v", res, err)
		}
		wantBackup := dst + ".20261124T150000Z.old"
		if res.OldBackup != wantBackup || string(ops.files[wantBackup]) != "v1" || string(ops.files[dst]) != "v2" {
			t.Fatalf("unexpected files/backup: res=%+v files=%v", res, ops.files)
		}
		if len(ops.aclSet) != 1 || ops.aclSet[0] != wantBackup || len(ops.rebootDel) != 1 || ops.rebootDel[0] != wantBackup {
			t.Fatalf("expected ACL + reboot delete on %s: acl=%v reboot=%v", wantBackup, ops.aclSet, ops.rebootDel)
		}
		if ops.stoppedCount != 0 {
			t.Fatalf("tier1 should not stop FrameServer, got %d", ops.stoppedCount)
		}
	})

	t.Run("tier2_fallback_on_rename_error", func(t *testing.T) {
		ops := &fakeDeployOps{
			files:     map[string][]byte{src: []byte("v2"), dst: []byte("v1")},
			renameErr: errors.New("sharing violation"),
			fixedNow:  fixed,
		}
		res, err := deployDLL(src, dst, UpgradeAuto, BackendMF, ops)
		if err != nil || res.Tier != "frameserver-restart" {
			t.Fatalf("tier2 fallback failed: res=%+v err=%v", res, err)
		}
		if ops.stoppedCount != 1 || ops.startedCount != 1 || string(ops.files[dst]) != "v2" {
			t.Fatalf("unexpected tier2 state: stop=%d start=%d dst=%q", ops.stoppedCount, ops.startedCount, ops.files[dst])
		}
	})

	t.Run("forced_rename_error", func(t *testing.T) {
		ops := &fakeDeployOps{
			files:     map[string][]byte{src: []byte("v2"), dst: []byte("v1")},
			renameErr: errors.New("locked"),
			fixedNow:  fixed,
		}
		if _, err := deployDLL(src, dst, UpgradeRename, BackendMF, ops); err == nil {
			t.Fatal("expected error when UpgradeRename fails")
		}
		if ops.stoppedCount != 0 {
			t.Fatalf("UpgradeRename must not stop FrameServer, got %d", ops.stoppedCount)
		}
	})

	t.Run("forced_restart_even_same_hash", func(t *testing.T) {
		ops := &fakeDeployOps{files: map[string][]byte{src: []byte("v1"), dst: []byte("v1")}, fixedNow: fixed}
		res, err := deployDLL(src, dst, UpgradeRestart, BackendMF, ops)
		if err != nil || res.Tier != "frameserver-restart" || ops.stoppedCount != 1 || ops.startedCount != 1 {
			t.Fatalf("forced restart failed: res=%+v err=%v stop=%d start=%d", res, err, ops.stoppedCount, ops.startedCount)
		}
	})

	t.Run("tier1_rollback_on_copy_error", func(t *testing.T) {
		ops := &fakeDeployOps{
			files:    map[string][]byte{src: []byte("v2"), dst: []byte("v1")},
			copyErr:  errors.New("disk full"),
			fixedNow: fixed,
		}
		if _, err := deployDLL(src, dst, UpgradeAuto, BackendMF, ops); err == nil {
			t.Fatal("expected error when copy fails")
		}
		if string(ops.files[dst]) != "v1" {
			t.Fatalf("expected rollback of %s to v1, got %q", dst, ops.files[dst])
		}
	})

	t.Run("uninstall_locked_rename_and_fallback", func(t *testing.T) {
		ops1 := &fakeDeployOps{
			files:     map[string][]byte{dst: []byte("v1")},
			removeErr: errors.New("access denied"),
			fixedNow:  fixed,
		}
		tier, err := removeInstalledDLL(dst, UpgradeAuto, BackendMF, ops1)
		if err != nil || tier != "rename-deferred" || len(ops1.rebootDel) != 1 {
			t.Fatalf("uninstall rename fallback failed: tier=%q err=%v reboot=%v", tier, err, ops1.rebootDel)
		}

		ops2 := &fakeDeployOps{
			files:     map[string][]byte{dst: []byte("v1")},
			removeErr: errors.New("locked"),
			renameErr: errors.New("locked"),
			fixedNow:  fixed,
		}
		// Once FrameServer stops, removeFile succeeds in real life; simulate by clearing removeErr via restart mode test or custom ops.
		if _, err := removeInstalledDLL(dst, UpgradeAuto, BackendMF, ops2); err == nil {
			t.Fatal("expected error when removeErr stays set after FrameServer stop")
		}
		if ops2.stoppedCount != 1 || ops2.startedCount != 1 {
			t.Fatalf("expected FrameServer stop+start on fallback, stop=%d start=%d", ops2.stoppedCount, ops2.startedCount)
		}
	})

	t.Run("dshow_deploy_never_touches_frameserver_and_explains_close_apps", func(t *testing.T) {
		ops := &fakeDeployOps{
			files:     map[string][]byte{src: []byte("v2"), dst: []byte("v1")},
			renameErr: errors.New("sharing violation"),
			fixedNow:  fixed,
		}
		_, err := deployDLL(src, dst, UpgradeAuto, BackendDShow, ops)
		if err == nil {
			t.Fatal("expected error when DirectShow tier-1 rename fails")
		}
		if ops.stoppedCount != 0 || ops.startedCount != 0 {
			t.Fatalf("BackendDShow must never stop/start FrameServer: stop=%d start=%d", ops.stoppedCount, ops.startedCount)
		}
		if !strings.Contains(err.Error(), "OBS") || !strings.Contains(err.Error(), "close applications") {
			t.Fatalf("expected actionable close-apps error message, got: %v", err)
		}

		// Even UpgradeRestart mode must not touch FrameServer for BackendDShow.
		opsRestart := &fakeDeployOps{
			files:     map[string][]byte{src: []byte("v2"), dst: []byte("v1")},
			renameErr: errors.New("locked"),
			fixedNow:  fixed,
		}
		if _, err := deployDLL(src, dst, UpgradeRestart, BackendDShow, opsRestart); err == nil {
			t.Fatal("expected error on BackendDShow UpgradeRestart when rename fails")
		}
		if opsRestart.stoppedCount != 0 || opsRestart.startedCount != 0 {
			t.Fatalf("BackendDShow UpgradeRestart must never touch FrameServer: stop=%d start=%d", opsRestart.stoppedCount, opsRestart.startedCount)
		}
	})

	t.Run("dshow_uninstall_never_touches_frameserver_and_explains_close_apps", func(t *testing.T) {
		ops := &fakeDeployOps{
			files:     map[string][]byte{dst: []byte("v1")},
			removeErr: errors.New("locked"),
			renameErr: errors.New("locked"),
			fixedNow:  fixed,
		}
		_, err := removeInstalledDLL(dst, UpgradeAuto, BackendDShow, ops)
		if err == nil {
			t.Fatal("expected error when DirectShow uninstall rename fails")
		}
		if ops.stoppedCount != 0 || ops.startedCount != 0 {
			t.Fatalf("BackendDShow uninstall must never stop/start FrameServer: stop=%d start=%d", ops.stoppedCount, ops.startedCount)
		}
		if !strings.Contains(err.Error(), "OBS") || !strings.Contains(err.Error(), "close applications") {
			t.Fatalf("expected actionable close-apps error message, got: %v", err)
		}
	})
}
