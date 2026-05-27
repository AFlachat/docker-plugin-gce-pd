package gce

import (
	"context"
	"errors"
	"testing"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

// --- fakes implementing diskAPI / instanceAPI / operation ---

// noopOp is an operation that completes instantly with a fixed result.
type noopOp struct{ err error }

func (o noopOp) Wait(ctx context.Context, _ ...gax.CallOption) error { return o.err }

type fakeDisks struct {
	// state keyed by disk name
	disks map[string]*computepb.Disk

	insertCalls int
	deleteCalls int
	snapCalls   int

	// insertErr lets a test force the Insert API call to fail.
	insertErr error
}

func newFakeDisks() *fakeDisks {
	return &fakeDisks{disks: map[string]*computepb.Disk{}}
}

func (f *fakeDisks) Insert(ctx context.Context, req *computepb.InsertDiskRequest, _ ...gax.CallOption) (operation, error) {
	f.insertCalls++
	if f.insertErr != nil {
		return nil, f.insertErr
	}
	d := req.GetDiskResource()
	// Simulate GCE: created disk becomes READY, detached.
	stored := proto.Clone(d).(*computepb.Disk)
	stored.Status = proto.String("READY")
	f.disks[d.GetName()] = stored
	return noopOp{}, nil
}

func (f *fakeDisks) Get(ctx context.Context, req *computepb.GetDiskRequest, _ ...gax.CallOption) (*computepb.Disk, error) {
	d, ok := f.disks[req.GetDisk()]
	if !ok {
		return nil, &googleapi.Error{Code: 404, Message: "not found"}
	}
	return d, nil
}

func (f *fakeDisks) Delete(ctx context.Context, req *computepb.DeleteDiskRequest, _ ...gax.CallOption) (operation, error) {
	f.deleteCalls++
	delete(f.disks, req.GetDisk())
	return noopOp{}, nil
}

func (f *fakeDisks) CreateSnapshot(ctx context.Context, req *computepb.CreateSnapshotDiskRequest, _ ...gax.CallOption) (operation, error) {
	f.snapCalls++
	return noopOp{}, nil
}

func (f *fakeDisks) List(ctx context.Context, req *computepb.ListDisksRequest) ([]*computepb.Disk, error) {
	var out []*computepb.Disk
	for _, d := range f.disks {
		if d.GetLabels()[ManagedByLabelKey] == ManagedByLabelValue {
			out = append(out, d)
		}
	}
	return out, nil
}

func (f *fakeDisks) Close() error { return nil }

type fakeInstances struct {
	attachCalls int
	detachCalls int
	// link back to the disk store so attach/detach can mutate Users.
	disks    *fakeDisks
	selfLink string

	// --- failover test knobs (zero values are inert) ---
	// status maps instance name -> GCE status returned by Get. Missing name with
	// statusNotFound=true simulates an absent instance.
	status         map[string]string
	statusNotFound map[string]bool
	// detachTargets records the instance each DetachDisk targeted, in order.
	detachTargets []string
	// detachReleasesAfter: if >0, DetachDisk only clears Users once this many
	// detach calls have happened (simulates a holder that releases late).
	detachReleasesAfter int
}

func (f *fakeInstances) AttachDisk(ctx context.Context, req *computepb.AttachDiskInstanceRequest, _ ...gax.CallOption) (operation, error) {
	f.attachCalls++
	name := req.GetAttachedDiskResource().GetDeviceName()
	if d, ok := f.disks.disks[name]; ok {
		d.Users = append(d.Users, f.selfLink)
	}
	return noopOp{}, nil
}

func (f *fakeInstances) DetachDisk(ctx context.Context, req *computepb.DetachDiskInstanceRequest, _ ...gax.CallOption) (operation, error) {
	f.detachCalls++
	f.detachTargets = append(f.detachTargets, req.GetInstance())
	if f.detachReleasesAfter > 0 && f.detachCalls < f.detachReleasesAfter {
		return noopOp{}, nil // holder hasn't released yet
	}
	if d, ok := f.disks.disks[req.GetDeviceName()]; ok {
		d.Users = nil
	}
	return noopOp{}, nil
}

func (f *fakeInstances) Get(ctx context.Context, req *computepb.GetInstanceRequest, _ ...gax.CallOption) (*computepb.Instance, error) {
	name := req.GetInstance()
	if f.statusNotFound[name] {
		return nil, &googleapi.Error{Code: 404, Message: "not found"}
	}
	st := f.status[name]
	return &computepb.Instance{Name: proto.String(name), Status: proto.String(st)}, nil
}

func (f *fakeInstances) Close() error { return nil }

// testClient wires a Client to the fakes with a fast backoff.
func testClient(t *testing.T) (*Client, *fakeDisks, *fakeInstances) {
	t.Helper()
	fd := newFakeDisks()
	cfg := Config{ProjectID: "proj", Zone: "europe-west1-b", Instance: "vm0",
		Backoff: fastBackoff(), OpTimeout: time.Second}
	c := newClient(cfg, fd, nil)
	fi := &fakeInstances{disks: fd, selfLink: c.instanceSelfLink()}
	c.instances = fi
	return c, fd, fi
}

func TestCreateDiskHappyPath(t *testing.T) {
	c, fd, _ := testClient(t)
	opts, _ := ParseDiskOptions(map[string]string{"size": "20", "type": "pd-ssd"})

	info, err := c.CreateDisk(context.Background(), "vol1", opts)
	if err != nil {
		t.Fatalf("CreateDisk error = %v", err)
	}
	if info.SizeGB != 20 || info.Type != "pd-ssd" || info.Status != "READY" {
		t.Errorf("disk info = %+v", info)
	}
	if info.Labels[ManagedByLabelKey] != ManagedByLabelValue {
		t.Errorf("managed-by label missing: %v", info.Labels)
	}
	if fd.insertCalls != 1 {
		t.Errorf("insertCalls = %d, want 1", fd.insertCalls)
	}
}

func TestCreateDiskRejectsBadName(t *testing.T) {
	c, fd, _ := testClient(t)
	opts, _ := ParseDiskOptions(nil)
	if _, err := c.CreateDisk(context.Background(), "Bad_Name", opts); err == nil {
		t.Fatal("expected name validation error")
	}
	if fd.insertCalls != 0 {
		t.Errorf("insert should not be called on invalid name, got %d", fd.insertCalls)
	}
}

func TestGetDiskNotFound(t *testing.T) {
	c, _, _ := testClient(t)
	_, err := c.GetDisk(context.Background(), "ghost")
	if !errors.Is(err, ErrDiskNotFound) {
		t.Errorf("err = %v, want ErrDiskNotFound", err)
	}
}

func TestDeleteRefusesAttachedDisk(t *testing.T) {
	c, _, _ := testClient(t)
	opts, _ := ParseDiskOptions(nil)
	if _, err := c.CreateDisk(context.Background(), "vol1", opts); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDisk(context.Background(), "vol1", false); err == nil {
		t.Fatal("expected refusal to delete attached disk")
	}
}

func TestDeleteIdempotentOnMissing(t *testing.T) {
	c, fd, _ := testClient(t)
	if err := c.DeleteDisk(context.Background(), "ghost", false); err != nil {
		t.Errorf("deleting missing disk should be nil, got %v", err)
	}
	if fd.deleteCalls != 0 {
		t.Errorf("should not call Delete API for missing disk, got %d", fd.deleteCalls)
	}
}

func TestDeleteWithSnapshot(t *testing.T) {
	c, fd, _ := testClient(t)
	opts, _ := ParseDiskOptions(nil)
	if _, err := c.CreateDisk(context.Background(), "vol1", opts); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDisk(context.Background(), "vol1", true); err != nil {
		t.Fatalf("DeleteDisk error = %v", err)
	}
	if fd.snapCalls != 1 {
		t.Errorf("snapCalls = %d, want 1", fd.snapCalls)
	}
	if fd.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", fd.deleteCalls)
	}
}

func TestAttachDetachIdempotent(t *testing.T) {
	c, _, fi := testClient(t)
	opts, _ := ParseDiskOptions(nil)
	if _, err := c.CreateDisk(context.Background(), "vol1", opts); err != nil {
		t.Fatal(err)
	}

	// Attach twice: second is a no-op.
	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatal(err)
	}
	if fi.attachCalls != 1 {
		t.Errorf("attachCalls = %d, want 1 (second attach should be no-op)", fi.attachCalls)
	}

	// Detach, then detach again: second is a no-op.
	if err := c.DetachDisk(context.Background(), "vol1"); err != nil {
		t.Fatal(err)
	}
	if err := c.DetachDisk(context.Background(), "vol1"); err != nil {
		t.Fatal(err)
	}
	if fi.detachCalls != 1 {
		t.Errorf("detachCalls = %d, want 1 (second detach should be no-op)", fi.detachCalls)
	}
}

func TestListManagedDisks(t *testing.T) {
	c, fd, _ := testClient(t)
	opts, _ := ParseDiskOptions(nil)
	if _, err := c.CreateDisk(context.Background(), "vol1", opts); err != nil {
		t.Fatal(err)
	}
	// Inject a foreign disk with no managed-by label; it must be excluded.
	fd.disks["foreign"] = &computepb.Disk{Name: proto.String("foreign"), Status: proto.String("READY")}

	got, err := c.ListManagedDisks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "vol1" {
		t.Errorf("ListManagedDisks = %+v, want only vol1", got)
	}
}

func TestCreateDiskInsertFailureSurfaces(t *testing.T) {
	c, fd, _ := testClient(t)
	fd.insertErr = &googleapi.Error{Code: 403, Message: "permission denied"}
	opts, _ := ParseDiskOptions(nil)
	_, err := c.CreateDisk(context.Background(), "vol1", opts)
	if err == nil {
		t.Fatal("expected insert error to surface")
	}
}
