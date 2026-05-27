package driver

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/docker/go-plugins-helpers/volume"

	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

// --- fakes ---

type fakeGCE struct {
	disks map[string]*gce.DiskInfo // name -> info (presence = exists)

	createErr error
	attachErr error

	attachCalls int
	detachCalls int
	deleteCalls int
	createCalls int

	attached map[string]bool
}

func newFakeGCE() *fakeGCE {
	return &fakeGCE{disks: map[string]*gce.DiskInfo{}, attached: map[string]bool{}}
}

func (f *fakeGCE) CreateDisk(_ context.Context, name string, opts gce.DiskOptions) (*gce.DiskInfo, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	info := &gce.DiskInfo{Name: name, SizeGB: opts.SizeGB, Type: opts.Type, Status: "READY"}
	f.disks[name] = info
	return info, nil
}
func (f *fakeGCE) GetDisk(_ context.Context, name string) (*gce.DiskInfo, error) {
	d, ok := f.disks[name]
	if !ok {
		return nil, gce.ErrDiskNotFound
	}
	return d, nil
}
func (f *fakeGCE) DeleteDisk(_ context.Context, name string, _ bool) error {
	f.deleteCalls++
	delete(f.disks, name)
	return nil
}
func (f *fakeGCE) ListManagedDisks(_ context.Context) ([]gce.DiskInfo, error) {
	out := make([]gce.DiskInfo, 0, len(f.disks))
	for _, d := range f.disks {
		out = append(out, *d)
	}
	return out, nil
}
func (f *fakeGCE) AttachDisk(_ context.Context, name string) error {
	f.attachCalls++
	if f.attachErr != nil {
		return f.attachErr
	}
	f.attached[name] = true
	return nil
}
func (f *fakeGCE) DetachDisk(_ context.Context, name string) error {
	f.detachCalls++
	f.attached[name] = false
	return nil
}

type fakeMount struct {
	mounted map[string]bool // target -> mounted

	waitErr   error
	formatErr error
	mountErr  error

	mountCalls   int
	unmountCalls int
}

func newFakeMount() *fakeMount { return &fakeMount{mounted: map[string]bool{}} }

func (f *fakeMount) WaitForDevice(_ context.Context, name string) (string, error) {
	if f.waitErr != nil {
		return "", f.waitErr
	}
	return "/dev/disk/by-id/google-" + name, nil
}
func (f *fakeMount) EnsureFormatted(_ context.Context, _, fsType string) (string, error) {
	if f.formatErr != nil {
		return "", f.formatErr
	}
	return fsType, nil
}
func (f *fakeMount) Mount(_ context.Context, _, target string) error {
	f.mountCalls++
	if f.mountErr != nil {
		return f.mountErr
	}
	f.mounted[target] = true
	return nil
}
func (f *fakeMount) Unmount(_ context.Context, target string) error {
	f.unmountCalls++
	f.mounted[target] = false
	return nil
}
func (f *fakeMount) IsMounted(target string) (bool, error) { return f.mounted[target], nil }

func newTestDriver(t *testing.T) (*Driver, *fakeGCE, *fakeMount, *state.Store) {
	t.Helper()
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := newFakeGCE()
	m := newFakeMount()
	return New(g, m, st, "local"), g, m, st
}

func createReq(name string, opts map[string]string) *volume.CreateRequest {
	return &volume.CreateRequest{Name: name, Options: opts}
}

// --- tests ---

func TestCreateThenGet(t *testing.T) {
	d, g, _, _ := newTestDriver(t)
	if err := d.Create(createReq("vol1", map[string]string{"size": "20", "type": "pd-ssd"})); err != nil {
		t.Fatal(err)
	}
	if g.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", g.createCalls)
	}
	resp, err := d.Get(&volume.GetRequest{Name: "vol1"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Volume.Name != "vol1" {
		t.Errorf("got %+v", resp.Volume)
	}
}

func TestCreateIdempotentSameOptions(t *testing.T) {
	d, g, _, _ := newTestDriver(t)
	req := createReq("vol1", map[string]string{"size": "20"})
	if err := d.Create(req); err != nil {
		t.Fatal(err)
	}
	if err := d.Create(req); err != nil {
		t.Fatalf("second identical Create should be a no-op, got %v", err)
	}
	if g.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no second GCE create)", g.createCalls)
	}
}

func TestCreateConflictingOptionsErrors(t *testing.T) {
	d, _, _, _ := newTestDriver(t)
	if err := d.Create(createReq("vol1", map[string]string{"size": "20"})); err != nil {
		t.Fatal(err)
	}
	err := d.Create(createReq("vol1", map[string]string{"size": "50"}))
	if err == nil {
		t.Fatal("Create with different options should error")
	}
}

// failAddStore wraps a real store but forces Add to fail, to drive the
// Create rollback path (GCE create succeeded, local persist did not).
type failAddStore struct {
	*state.Store
	addErr error
}

func (s failAddStore) Add(v state.Volume) error { return s.addErr }

func TestCreateRollsBackOnStateFailure(t *testing.T) {
	realStore, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	g := newFakeGCE()
	m := newFakeMount()
	d := New(g, m, failAddStore{Store: realStore, addErr: errors.New("disk full")}, "local")

	err = d.Create(createReq("vol1", map[string]string{"size": "20"}))
	if err == nil {
		t.Fatal("Create should fail when state Add fails")
	}
	// The disk was created in GCE, then the failed Add must trigger a rollback.
	if g.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", g.createCalls)
	}
	if g.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1 (rollback of orphaned disk)", g.deleteCalls)
	}
	if _, ok := g.disks["vol1"]; ok {
		t.Error("disk should have been rolled back (deleted) in GCE")
	}
}

func TestMountUnmountRefCounting(t *testing.T) {
	d, g, m, st := newTestDriver(t)
	if err := d.Create(createReq("vol1", nil)); err != nil {
		t.Fatal(err)
	}

	// First mount: attach + format + mount + refCount 1.
	resp, err := d.Mount(&volume.MountRequest{Name: "vol1", ID: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	want := mountpointFor("vol1")
	if resp.Mountpoint != want {
		t.Errorf("mountpoint = %q, want %q", resp.Mountpoint, want)
	}
	if g.attachCalls != 1 || m.mountCalls != 1 {
		t.Errorf("attach=%d mount=%d, want 1/1", g.attachCalls, m.mountCalls)
	}

	// Second mount (another container): no new attach/mount, refCount 2.
	if _, err := d.Mount(&volume.MountRequest{Name: "vol1", ID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if g.attachCalls != 1 || m.mountCalls != 1 {
		t.Errorf("second mount should not re-attach/re-mount: attach=%d mount=%d", g.attachCalls, m.mountCalls)
	}
	v, _ := st.Get("vol1")
	if v.RefCount != 2 {
		t.Errorf("refCount = %d, want 2", v.RefCount)
	}

	// First unmount: still in use, no detach.
	if err := d.Unmount(&volume.UnmountRequest{Name: "vol1", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if g.detachCalls != 0 || m.unmountCalls != 0 {
		t.Errorf("detach should not happen while refCount>0: detach=%d unmount=%d", g.detachCalls, m.unmountCalls)
	}

	// Second unmount: refCount hits 0 -> unmount + detach.
	if err := d.Unmount(&volume.UnmountRequest{Name: "vol1", ID: "c2"}); err != nil {
		t.Fatal(err)
	}
	if g.detachCalls != 1 || m.unmountCalls != 1 {
		t.Errorf("final unmount should detach: detach=%d unmount=%d", g.detachCalls, m.unmountCalls)
	}
	v, _ = st.Get("vol1")
	if v.RefCount != 0 || v.Status != state.StatusCreated {
		t.Errorf("after full unmount: %+v", v)
	}
}

func TestMountAttachFailureNoLeak(t *testing.T) {
	d, g, m, _ := newTestDriver(t)
	_ = d.Create(createReq("vol1", nil))
	g.attachErr = errors.New("attach boom")

	if _, err := d.Mount(&volume.MountRequest{Name: "vol1", ID: "c1"}); err == nil {
		t.Fatal("expected mount to fail")
	}
	if m.mountCalls != 0 {
		t.Error("should not mount after attach failure")
	}
}

func TestMountDeviceWaitFailureDetaches(t *testing.T) {
	d, g, m, _ := newTestDriver(t)
	_ = d.Create(createReq("vol1", nil))
	m.waitErr = errors.New("device never appeared")

	if _, err := d.Mount(&volume.MountRequest{Name: "vol1", ID: "c1"}); err == nil {
		t.Fatal("expected mount to fail")
	}
	if g.detachCalls != 1 {
		t.Errorf("device-wait failure should trigger detach, detachCalls = %d", g.detachCalls)
	}
}

func TestRemoveInUseFails(t *testing.T) {
	d, _, _, _ := newTestDriver(t)
	_ = d.Create(createReq("vol1", nil))
	if _, err := d.Mount(&volume.MountRequest{Name: "vol1", ID: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove(&volume.RemoveRequest{Name: "vol1"}); err == nil {
		t.Fatal("Remove of in-use volume should fail")
	}
}

func TestRemoveDeletePolicyDeletesDiskAndState(t *testing.T) {
	d, g, _, st := newTestDriver(t)
	_ = d.Create(createReq("vol1", map[string]string{"reclaimPolicy": "delete"}))
	if err := d.Remove(&volume.RemoveRequest{Name: "vol1"}); err != nil {
		t.Fatal(err)
	}
	if g.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", g.deleteCalls)
	}
	if _, ok := st.Get("vol1"); ok {
		t.Error("state record should be gone after Remove")
	}
}

func TestRemoveRetainPolicyKeepsDisk(t *testing.T) {
	d, g, _, st := newTestDriver(t)
	// No reclaimPolicy opt -> defaults to retain.
	_ = d.Create(createReq("vol1", nil))
	if err := d.Remove(&volume.RemoveRequest{Name: "vol1"}); err != nil {
		t.Fatal(err)
	}
	if g.deleteCalls != 0 {
		t.Errorf("retain policy must NOT delete the PD, deleteCalls = %d", g.deleteCalls)
	}
	// The disk is still in GCE.
	if _, ok := g.disks["vol1"]; !ok {
		t.Error("retained disk should still exist in GCE")
	}
	// But the local record is dropped.
	if _, ok := st.Get("vol1"); ok {
		t.Error("local record should be dropped on Remove even with retain")
	}
}

func TestCapabilitiesScopeLocal(t *testing.T) {
	d, _, _, _ := newTestDriver(t)
	if d.Capabilities().Capabilities.Scope != "local" {
		t.Error("scope should be local for zonal PDs")
	}
}

func TestCapabilitiesScopeGlobal(t *testing.T) {
	st, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := New(newFakeGCE(), newFakeMount(), st, "global")
	if d.Capabilities().Capabilities.Scope != "global" {
		t.Error("scope should be global when configured for Swarm")
	}
}

func TestNewDefaultsScopeToLocal(t *testing.T) {
	st, _ := state.Load(filepath.Join(t.TempDir(), "state.json"))
	d := New(newFakeGCE(), newFakeMount(), st, "")
	if d.Capabilities().Capabilities.Scope != "local" {
		t.Error("empty scope should default to local")
	}
}

func TestReconcileImportsAndRestoresMounts(t *testing.T) {
	d, g, m, st := newTestDriver(t)

	// GCE has a disk the local state never heard of.
	g.disks["orphan"] = &gce.DiskInfo{Name: "orphan", SizeGB: 30, Type: "pd-balanced", Status: "READY"}

	// Local state has a volume whose disk is gone (phantom) and one still mounted.
	_ = st.Add(state.Volume{Name: "phantom", Status: state.StatusCreated})
	_ = st.Add(state.Volume{Name: "live", Status: state.StatusMounted, RefCount: 7})
	g.disks["live"] = &gce.DiskInfo{Name: "live", SizeGB: 10, Type: "pd-ssd", Status: "READY"}
	m.mounted[mountpointFor("live")] = true // still mounted on disk

	if err := d.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}

	// orphan imported
	if _, ok := st.Get("orphan"); !ok {
		t.Error("orphan disk should be imported")
	}
	// phantom removed
	if _, ok := st.Get("phantom"); ok {
		t.Error("phantom volume should be removed")
	}
	// live: refCount reset from 7 then restored to 1 (still mounted)
	live, _ := st.Get("live")
	if live.RefCount != 1 || live.Status != state.StatusMounted {
		t.Errorf("live after reconcile = %+v, want refCount 1 mounted", live)
	}
}
