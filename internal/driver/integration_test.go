//go:build integration

// Package driver integration test.
//
// This test runs a full create / mount / write / read / unmount / remove cycle
// against REAL GCE infrastructure. It is excluded from normal builds by the
// `integration` build tag and must be run explicitly, on a GCE VM, by an
// operator with the IAM permissions listed in the README.
//
// Run it with:
//
//	sudo -E go test -tags=integration -run TestIntegrationFullCycle \
//	    -v -timeout 10m ./internal/driver/
//
// `sudo` is required because the test attaches/formats/mounts a real disk.
// `-E` preserves GCEPD_KEYFILE / GOOGLE_APPLICATION_CREDENTIALS if you use them.
//
// It creates a disk named "gcepd-itest-<pid>" and deletes it at the end; if the
// test is interrupted, delete the disk manually:
//
//	gcloud compute disks delete gcepd-itest-<pid> --zone <zone>
package driver

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/go-plugins-helpers/volume"

	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/metadata"
	"github.com/aflachat/docker-plugin-gce-pd/internal/mount"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

func TestIntegrationFullCycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// Must be on GCE.
	md := metadata.New()
	if !md.OnGCE(ctx) {
		t.Skip("not running on a GCE VM; skipping integration test")
	}
	id, err := md.Fetch(ctx)
	if err != nil {
		t.Fatalf("fetch metadata: %v", err)
	}
	t.Logf("GCE: project=%s zone=%s instance=%s", id.ProjectID, id.Zone, id.InstanceName)

	gceClient, err := gce.New(ctx, gce.Config{
		ProjectID: id.ProjectID, Zone: id.Zone, Instance: id.InstanceName,
	})
	if err != nil {
		t.Fatalf("gce client: %v", err)
	}
	defer gceClient.Close()

	store, err := state.Load(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	d := New(gceClient, mount.New(), store, "local")

	volName := fmt.Sprintf("gcepd-itest-%d", os.Getpid())
	t.Logf("test volume: %s", volName)

	// Best-effort cleanup even on failure.
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer ccancel()
		_ = d.Unmount(&volume.UnmountRequest{Name: volName, ID: "itest"})
		if err := gceClient.DeleteDisk(cctx, volName, false); err != nil {
			t.Logf("cleanup: delete disk %s: %v (you may need to delete it manually)", volName, err)
		}
	})

	// 1. Create
	if err := d.Create(&volume.CreateRequest{
		Name:    volName,
		Options: map[string]string{"size": "10", "type": "pd-balanced", "fs": "ext4"},
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// 2. Mount
	resp, err := d.Mount(&volume.MountRequest{Name: volName, ID: "itest"})
	if err != nil {
		t.Fatalf("Mount: %v", err)
	}
	mp := resp.Mountpoint
	t.Logf("mounted at %s", mp)

	// 3. Write + read back a file to prove the filesystem works.
	testFile := filepath.Join(mp, "hello.txt")
	want := "gcepd integration ok"
	if err := os.WriteFile(testFile, []byte(want), 0o644); err != nil {
		t.Fatalf("write to mounted volume: %v", err)
	}
	got, err := os.ReadFile(testFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != want {
		t.Fatalf("read %q, want %q", got, want)
	}

	// 4. Verify it's actually mounted per the OS.
	if out, err := exec.CommandContext(ctx, "findmnt", mp).CombinedOutput(); err != nil {
		t.Fatalf("findmnt %s failed (not mounted?): %v\n%s", mp, err, out)
	}

	// 5. Unmount (refCount -> 0 -> detach)
	if err := d.Unmount(&volume.UnmountRequest{Name: volName, ID: "itest"}); err != nil {
		t.Fatalf("Unmount: %v", err)
	}

	// 6. Remove (deletes the PD)
	if err := d.Remove(&volume.RemoveRequest{Name: volName}); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// 7. Confirm the disk is gone.
	if _, err := gceClient.GetDisk(ctx, volName); err == nil {
		t.Fatalf("disk %s still exists after Remove", volName)
	}
}
