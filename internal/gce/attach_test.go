package gce

import (
	"context"
	"strings"
	"testing"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// failoverClient builds a Client in the given scope wired to fakes, with a
// virtual clock so the grace loop never sleeps in real time. The disk starts
// attached to `holder`.
func failoverClient(t *testing.T, scope string, holder string, holderStatus string, holderAbsent bool) (*Client, *fakeDisks, *fakeInstances) {
	t.Helper()
	fd := newFakeDisks()
	// Seed a disk already attached to the holder instance.
	holderLink := "projects/proj/zones/europe-west1-b/instances/" + holder
	fd.disks["vol1"] = &computepb.Disk{
		Name:     proto.String("vol1"),
		Status:   proto.String("READY"),
		SelfLink: proto.String("projects/proj/zones/europe-west1-b/disks/vol1"),
		Users:    []string{holderLink},
	}

	cfg := Config{
		ProjectID: "proj", Zone: "europe-west1-b", Instance: "vm-self",
		Scope: scope, ForceDetachAfter: 5 * time.Second,
		Backoff: fastBackoff(), OpTimeout: time.Second,
	}
	c := newClient(cfg, fd, nil)

	fi := &fakeInstances{
		disks:          fd,
		selfLink:       c.instanceSelfLink(),
		status:         map[string]string{holder: holderStatus},
		statusNotFound: map[string]bool{holder: holderAbsent},
	}
	c.instances = fi

	// Virtual clock: advance time on every sleep so deadlines are reached
	// deterministically without real waiting.
	vnow := time.Unix(0, 0)
	c.now = func() time.Time { return vnow }
	c.sleep = func(d time.Duration) { vnow = vnow.Add(d) }

	return c, fd, fi
}

func attachedTo(d *computepb.Disk) []string { return d.GetUsers() }

func TestAttachLocalScopeRefusesForeignHolder(t *testing.T) {
	c, _, fi := failoverClient(t, ScopeLocal, "vm-other", "RUNNING", false)
	err := c.AttachDisk(context.Background(), "vol1")
	if err == nil {
		t.Fatal("local scope must refuse a disk attached elsewhere")
	}
	if !strings.Contains(err.Error(), "single-writer") {
		t.Errorf("err = %v", err)
	}
	if fi.detachCalls != 0 || fi.attachCalls != 0 {
		t.Errorf("local scope must not detach/attach: detach=%d attach=%d", fi.detachCalls, fi.attachCalls)
	}
}

func TestAttachGlobalHolderTerminatedDetachesImmediately(t *testing.T) {
	c, fd, fi := failoverClient(t, ScopeGlobal, "vm-other", "TERMINATED", false)
	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	if fi.detachCalls != 1 {
		t.Errorf("terminated holder should be detached exactly once, got %d", fi.detachCalls)
	}
	if len(fi.detachTargets) != 1 || fi.detachTargets[0] != "vm-other" {
		t.Errorf("detach should target the holder vm-other, got %v", fi.detachTargets)
	}
	if fi.attachCalls != 1 {
		t.Errorf("disk should be attached to self once, got %d", fi.attachCalls)
	}
	// Disk ends up attached to self.
	users := attachedTo(fd.disks["vol1"])
	if len(users) != 1 || !sameInstance(users[0], c.instanceSelfLink()) {
		t.Errorf("disk users = %v, want attached to self", users)
	}
}

func TestAttachGlobalHolderAbsentDetachesImmediately(t *testing.T) {
	c, _, fi := failoverClient(t, ScopeGlobal, "vm-gone", "", true)
	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	if fi.detachCalls != 1 {
		t.Errorf("absent holder should be detached once, got %d", fi.detachCalls)
	}
}

func TestAttachGlobalRunningHolderReleasesWithinGrace(t *testing.T) {
	c, _, fi := failoverClient(t, ScopeGlobal, "vm-other", "RUNNING", false)
	// Clean detach releases on the first call.
	fi.detachReleasesAfter = 0

	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	// Exactly one (clean) detach, no forced re-issue.
	if fi.detachCalls != 1 {
		t.Errorf("running holder that releases cleanly should detach once, got %d", fi.detachCalls)
	}
	if fi.attachCalls != 1 {
		t.Errorf("attach to self once, got %d", fi.attachCalls)
	}
}

func TestAttachGlobalRunningHolderNeverReleasesForces(t *testing.T) {
	c, fd, fi := failoverClient(t, ScopeGlobal, "vm-other", "RUNNING", false)
	// Never release via the clean path: only a later (forced) detach clears it.
	// detachReleasesAfter=2 means the 1st detach (clean) does nothing; the 2nd
	// (forced, after grace) releases.
	fi.detachReleasesAfter = 2

	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	if fi.detachCalls < 2 {
		t.Errorf("running holder that won't release should be force-detached (>=2 detach calls), got %d", fi.detachCalls)
	}
	users := attachedTo(fd.disks["vol1"])
	if len(users) != 1 || !sameInstance(users[0], c.instanceSelfLink()) {
		t.Errorf("disk users = %v, want attached to self after force", users)
	}
}

func TestAttachGlobalAlreadyAttachedSelfNoop(t *testing.T) {
	c, fd, fi := failoverClient(t, ScopeGlobal, "vm-other", "RUNNING", false)
	// Override: disk already attached to self.
	fd.disks["vol1"].Users = []string{c.instanceSelfLink()}

	if err := c.AttachDisk(context.Background(), "vol1"); err != nil {
		t.Fatalf("AttachDisk: %v", err)
	}
	if fi.detachCalls != 0 || fi.attachCalls != 0 {
		t.Errorf("already attached to self should be a no-op: detach=%d attach=%d", fi.detachCalls, fi.attachCalls)
	}
}
