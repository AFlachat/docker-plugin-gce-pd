package mount

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// --- fakes ---

type cmdResult struct {
	out string
	err error
}

type fakeRunner struct {
	// results keyed by command name (the program, e.g. "blkid", "mount").
	results map[string]cmdResult
	calls   [][]string // each entry is [name, arg, arg, ...]
}

func (f *fakeRunner) run(_ context.Context, name string, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string{name}, args...))
	r := f.results[name]
	return r.out, r.err
}

func (f *fakeRunner) called(name string) bool {
	for _, c := range f.calls {
		if c[0] == name {
			return true
		}
	}
	return false
}

type fakeFS struct {
	existing map[string]bool // paths that exist
	mounts   string          // /proc/mounts content
	mkdirs   []string
}

func (f *fakeFS) stat(path string) error {
	if f.existing[path] {
		return nil
	}
	return os.ErrNotExist
}
func (f *fakeFS) mkdirAll(path string, _ os.FileMode) error {
	f.mkdirs = append(f.mkdirs, path)
	return nil
}
func (f *fakeFS) readMounts() (string, error) { return f.mounts, nil }

// fakeClock advances deterministically: now() returns a moving time, sleep
// advances it. This lets WaitForDevice time out without real waiting.
type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

func newMounter(r *fakeRunner, fs *fakeFS, clk *fakeClock) *Mounter {
	return &Mounter{run: r, fs: fs, clk: clk, deviceTimeout: 5 * time.Second}
}

// --- tests ---

func TestProbeFSTypeFormatted(t *testing.T) {
	r := &fakeRunner{results: map[string]cmdResult{
		"blkid": {out: "ext4\n"},
	}}
	m := newMounter(r, &fakeFS{}, &fakeClock{})
	fsType, err := m.ProbeFSType(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatal(err)
	}
	if fsType != "ext4" {
		t.Errorf("fsType = %q, want ext4", fsType)
	}
}

func TestProbeFSTypeBlankDevice(t *testing.T) {
	// blkid exits 2 on a blank device — must be treated as "no filesystem".
	r := &fakeRunner{results: map[string]cmdResult{
		"blkid": {out: "", err: &exitError{cmd: "blkid", code: 2}},
	}}
	m := newMounter(r, &fakeFS{}, &fakeClock{})
	fsType, err := m.ProbeFSType(context.Background(), "/dev/sdb")
	if err != nil {
		t.Fatalf("blank device should not error, got %v", err)
	}
	if fsType != "" {
		t.Errorf("fsType = %q, want empty", fsType)
	}
}

func TestProbeFSTypeRealError(t *testing.T) {
	r := &fakeRunner{results: map[string]cmdResult{
		"blkid": {err: &exitError{cmd: "blkid", code: 4}},
	}}
	m := newMounter(r, &fakeFS{}, &fakeClock{})
	if _, err := m.ProbeFSType(context.Background(), "/dev/sdb"); err == nil {
		t.Fatal("blkid exit 4 should surface as error")
	}
}

func TestEnsureFormattedBlankGetsFormatted(t *testing.T) {
	r := &fakeRunner{results: map[string]cmdResult{
		"blkid":     {err: &exitError{code: 2}}, // blank
		"mkfs.ext4": {out: "done"},
	}}
	m := newMounter(r, &fakeFS{}, &fakeClock{})
	got, err := m.EnsureFormatted(context.Background(), "/dev/sdb", "ext4")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ext4" {
		t.Errorf("got %q, want ext4", got)
	}
	if !r.called("mkfs.ext4") {
		t.Error("expected mkfs.ext4 to be called on blank device")
	}
}

func TestEnsureFormattedExistingNeverReformats(t *testing.T) {
	r := &fakeRunner{results: map[string]cmdResult{
		"blkid": {out: "xfs"},
	}}
	m := newMounter(r, &fakeFS{}, &fakeClock{})
	got, err := m.EnsureFormatted(context.Background(), "/dev/sdb", "ext4")
	if err != nil {
		t.Fatal(err)
	}
	if got != "xfs" {
		t.Errorf("got %q, want xfs (existing fs preserved)", got)
	}
	if r.called("mkfs.ext4") || r.called("mkfs.xfs") {
		t.Fatal("must NOT reformat a device that already has a filesystem")
	}
}

func TestMkfsArgs(t *testing.T) {
	ext4, _ := mkfsArgs("ext4", "/dev/sdb")
	if strings.Join(ext4, " ") != "mkfs.ext4 -F /dev/sdb" {
		t.Errorf("ext4 args = %v", ext4)
	}
	xfs, _ := mkfsArgs("xfs", "/dev/sdb")
	if strings.Join(xfs, " ") != "mkfs.xfs -f /dev/sdb" {
		t.Errorf("xfs args = %v", xfs)
	}
	if _, err := mkfsArgs("btrfs", "/dev/sdb"); err == nil {
		t.Error("unsupported fs should error")
	}
}

func TestMountIdempotentAndCreatesDir(t *testing.T) {
	target := "/var/lib/docker-gcepd/mounts/vol1"
	fs := &fakeFS{mounts: ""} // nothing mounted
	r := &fakeRunner{results: map[string]cmdResult{"mount": {}}}
	m := newMounter(r, fs, &fakeClock{})

	if err := m.Mount(context.Background(), "/dev/sdb", target); err != nil {
		t.Fatal(err)
	}
	if len(fs.mkdirs) != 1 || fs.mkdirs[0] != target {
		t.Errorf("mkdirs = %v, want [%s]", fs.mkdirs, target)
	}
	if !r.called("mount") {
		t.Error("expected mount to be called")
	}

	// Now pretend it's mounted; second Mount must be a no-op.
	fs.mounts = "/dev/sdb " + target + " ext4 rw 0 0"
	r2 := &fakeRunner{results: map[string]cmdResult{"mount": {}}}
	m2 := newMounter(r2, fs, &fakeClock{})
	if err := m2.Mount(context.Background(), "/dev/sdb", target); err != nil {
		t.Fatal(err)
	}
	if r2.called("mount") {
		t.Error("mount should be a no-op when already mounted")
	}
}

func TestUnmountIdempotent(t *testing.T) {
	target := "/var/lib/docker-gcepd/mounts/vol1"
	// Not mounted: umount must not be called.
	fs := &fakeFS{mounts: ""}
	r := &fakeRunner{results: map[string]cmdResult{"umount": {}}}
	m := newMounter(r, fs, &fakeClock{})
	if err := m.Unmount(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if r.called("umount") {
		t.Error("umount should be a no-op when not mounted")
	}

	// Mounted: umount called.
	fs.mounts = "/dev/sdb " + target + " ext4 rw 0 0"
	r2 := &fakeRunner{results: map[string]cmdResult{"umount": {}}}
	m2 := newMounter(r2, fs, &fakeClock{})
	if err := m2.Unmount(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if !r2.called("umount") {
		t.Error("umount should run when mounted")
	}
}

func TestWaitForDeviceAppears(t *testing.T) {
	dev := DevicePath("vol1")
	fs := &fakeFS{existing: map[string]bool{dev: true}}
	m := newMounter(&fakeRunner{}, fs, &fakeClock{})
	got, err := m.WaitForDevice(context.Background(), "vol1")
	if err != nil {
		t.Fatal(err)
	}
	if got != dev {
		t.Errorf("got %q, want %q", got, dev)
	}
}

func TestWaitForDeviceTimeout(t *testing.T) {
	fs := &fakeFS{existing: map[string]bool{}} // device never appears
	m := newMounter(&fakeRunner{}, fs, &fakeClock{})
	_, err := m.WaitForDevice(context.Background(), "vol1")
	if err == nil {
		t.Fatal("expected timeout error when device never appears")
	}
	if !strings.Contains(err.Error(), "did not appear") {
		t.Errorf("err = %v", err)
	}
}

func TestIsMountedExactMatch(t *testing.T) {
	// A path that is a prefix of a real mount must NOT be reported as mounted.
	fs := &fakeFS{mounts: "/dev/sdb /var/lib/docker-gcepd/mounts/vol10 ext4 rw 0 0"}
	m := newMounter(&fakeRunner{}, fs, &fakeClock{})
	mounted, err := m.IsMounted("/var/lib/docker-gcepd/mounts/vol1")
	if err != nil {
		t.Fatal(err)
	}
	if mounted {
		t.Error("vol1 must not match vol10 (prefix collision)")
	}
}
