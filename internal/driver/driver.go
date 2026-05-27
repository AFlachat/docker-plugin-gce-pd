// Package driver implements docker's volume.Driver interface, orchestrating the
// GCE, mount and state packages into the create/mount/unmount/remove lifecycle.
//
// Responsibilities layered here (not in the lower packages):
//   - idempotent Create with option-match checking
//   - rollback of a GCE disk when local state write fails
//   - reference counting across Mount/Unmount to decide attach/detach
//   - startup reconciliation of local state with GCE + /proc/mounts
package driver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/docker/go-plugins-helpers/volume"

	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

// MountRoot is the base directory under which each volume is mounted at
// MountRoot/<volume-name>.
const MountRoot = "/var/lib/docker-gcepd/mounts"

// opTimeout bounds a single driver operation end-to-end (GCE op + device wait +
// mkfs + mount). Individual sub-steps have their own tighter timeouts.
const opTimeout = 5 * time.Minute

// gceClient is the subset of *gce.Client the driver needs, extracted as an
// interface so the driver is unit-testable with a fake.
type gceClient interface {
	CreateDisk(ctx context.Context, name string, opts gce.DiskOptions) (*gce.DiskInfo, error)
	GetDisk(ctx context.Context, name string) (*gce.DiskInfo, error)
	DeleteDisk(ctx context.Context, name string, snapshotFirst bool) error
	ListManagedDisks(ctx context.Context) ([]gce.DiskInfo, error)
	AttachDisk(ctx context.Context, name string) error
	DetachDisk(ctx context.Context, name string) error
}

// mounter is the subset of *mount.Mounter the driver needs.
type mounter interface {
	WaitForDevice(ctx context.Context, volumeName string) (string, error)
	EnsureFormatted(ctx context.Context, device, fsType string) (string, error)
	Mount(ctx context.Context, device, target string) error
	Unmount(ctx context.Context, target string) error
	IsMounted(target string) (bool, error)
}

// stateStore is the subset of *state.Store the driver needs, as an interface so
// failure paths (e.g. a failing Add for rollback) are unit-testable.
type stateStore interface {
	Get(name string) (state.Volume, bool)
	List() []state.Volume
	Add(v state.Volume) error
	Remove(name string) error
	IncRef(name, mountpoint string) (int, error)
	DecRef(name string) (int, error)
	ResetRefCounts() error
	SetMounted(name, mountpoint string) error
	Reconcile(gceDiskNames []string, importOptions func(name string) state.VolumeOptions) (state.ReconcileResult, error)
}

// Driver implements volume.Driver.
type Driver struct {
	gce   gceClient
	mount mounter
	state stateStore

	// scope is reported by Capabilities: "local" (default) or "global" (Swarm).
	scope string

	// mu serializes whole driver operations on a per-volume basis. The state
	// store has its own fine-grained lock, but multi-step operations (create
	// then persist, attach then mount then incref) must not interleave for the
	// same volume; a single driver-wide lock is simplest and correct given the
	// low call rate from docker.
	mu sync.Mutex

	// bg tracks background work (e.g. delete-after-Remove) so it can be awaited
	// on shutdown.
	bg sync.WaitGroup
}

// New builds a Driver from its collaborators. *state.Store satisfies
// stateStore. scope is the volume capability scope ("local" or "global");
// empty defaults to "local".
func New(g gceClient, m mounter, s stateStore, scope string) *Driver {
	if scope == "" {
		scope = "local"
	}
	return &Driver{gce: g, mount: m, state: s, scope: scope}
}

// mountpointFor returns the mount target for a volume.
func mountpointFor(name string) string {
	return filepath.Join(MountRoot, name)
}

// ---- volume.Driver implementation ----

// Create provisions a GCE disk and records it. It is idempotent against both
// local state and GCE reality: a repeat Create with matching options is a no-op,
// and a disk that already exists in GCE (e.g. a retained PD not yet re-imported,
// or one created out-of-band) is adopted rather than re-created. Conflicting
// options error. The disk is NOT attached here (lazy attach at Mount).
func (d *Driver) Create(req *volume.CreateRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	opts, err := gce.ParseDiskOptions(req.Options)
	if err != nil {
		return err
	}
	if err := gce.ValidateDiskName(req.Name); err != nil {
		return err
	}

	// Idempotency vs local state: if we already track this volume, compare options.
	if existing, ok := d.state.Get(req.Name); ok {
		if optionsMatch(existing.Options, opts) {
			return nil
		}
		return fmt.Errorf("volume %q already exists with different options", req.Name)
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	// Idempotency vs GCE: the disk may exist already (a retained PD we haven't
	// re-imported yet, or one left over from a previous run). Adopt it instead of
	// failing with a 409.
	if existing, err := d.gce.GetDisk(ctx, req.Name); err == nil {
		// A disk marked for deletion is being torn down (delete in progress, or
		// interrupted and pending reconciliation). Refuse rather than adopt a
		// half-deleted disk or race the delete.
		if existing.Deleting() {
			return fmt.Errorf("volume %q is still being deleted; retry once the delete completes", req.Name)
		}
		return d.adoptExistingDisk(req.Name, existing, opts)
	} else if !errors.Is(err, gce.ErrDiskNotFound) {
		return err
	}

	info, err := d.gce.CreateDisk(ctx, req.Name, opts)
	if err != nil {
		return err
	}

	// Rollback the disk if we cannot persist the state, so we don't leak an
	// untracked PD.
	rec := state.Volume{
		Name:      req.Name,
		Options:   toStateOptions(opts),
		Status:    state.StatusCreated,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.state.Add(rec); err != nil {
		if delErr := d.gce.DeleteDisk(ctx, req.Name, false); delErr != nil {
			return fmt.Errorf("create %q: state write failed (%v) AND rollback failed (%v); "+
				"a disk may be orphaned in GCE", req.Name, err, delErr)
		}
		return fmt.Errorf("create %q: state write failed, disk rolled back: %w", req.Name, err)
	}
	_ = info // info already reflected in opts; kept for future status reporting
	return nil
}

// adoptExistingDisk records an already-existing GCE disk into local state under
// the requested options, instead of trying to (re-)create it. It only adopts a
// disk this plugin manages (carrying the managed-by label) and whose physical
// attributes (size, type) are compatible with the requested options; otherwise
// it errors rather than silently taking over a foreign or mismatched disk.
func (d *Driver) adoptExistingDisk(name string, info *gce.DiskInfo, opts gce.DiskOptions) error {
	if info.Labels[gce.ManagedByLabelKey] != gce.ManagedByLabelValue {
		return fmt.Errorf("disk %q already exists in GCE but is not managed by this plugin "+
			"(missing %s=%s label); refusing to adopt it", name, gce.ManagedByLabelKey, gce.ManagedByLabelValue)
	}
	if info.SizeGB != opts.SizeGB || info.Type != opts.Type {
		return fmt.Errorf("disk %q already exists in GCE with size=%dGiB type=%s, "+
			"which differs from the requested size=%dGiB type=%s",
			name, info.SizeGB, info.Type, opts.SizeGB, opts.Type)
	}

	// Compatible: persist a local record so Mount/Remove work. FSType isn't
	// readable from GCE metadata, so we keep the requested value; EnsureFormatted
	// probes the real filesystem at Mount and never reformats a non-blank disk.
	rec := state.Volume{
		Name:      name,
		Options:   toStateOptions(opts),
		Status:    state.StatusCreated,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := d.state.Add(rec); err != nil {
		return fmt.Errorf("adopt existing disk %q: %w", name, err)
	}
	log.Printf("gcepd: adopted pre-existing disk %q into local state", name)
	return nil
}

// Remove handles `docker volume rm`. It refuses a volume still in use (refCount
// > 0). What happens to the backing PD depends on the volume's reclaim policy:
//
//	retain (default) — the PD is kept in GCE (still labelled managed-by); only the
//	                    local record is dropped. Data survives and the disk is
//	                    re-imported at the next plugin startup.
//	delete           — the PD is deleted from GCE (optionally snapshotted first).
func (d *Driver) Remove(req *volume.RemoveRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	v, ok := d.state.Get(req.Name)
	if !ok {
		return fmt.Errorf("volume %q not found", req.Name)
	}
	if v.RefCount > 0 {
		return fmt.Errorf("volume %q is in use by %d mount(s)", req.Name, v.RefCount)
	}

	if v.Options.ReclaimPolicy == gce.ReclaimRetain {
		// Keep the disk; just forget it locally. It will reappear via startup
		// reconciliation, and a `docker volume create` of the same name reuses it.
		log.Printf("gcepd: volume %q removed with reclaimPolicy=retain; PD kept in GCE", req.Name)
		return d.state.Remove(req.Name)
	}

	// reclaimPolicy=delete. Snapshotting and deleting a PD can take longer than
	// the Docker daemon's plugin-request timeout, which would surface to the user
	// as "context deadline exceeded" even though the work is progressing. So we
	// drop the local record and acknowledge immediately, then run the GCE delete
	// in the background with our own generous timeout.
	//
	// Safety: the disk keeps its managed-by label until actually deleted, so if
	// the background op fails (or the plugin restarts mid-delete), startup
	// reconciliation re-imports it — the disk is never silently lost.
	if err := d.state.Remove(req.Name); err != nil {
		return err
	}
	d.deleteInBackground(req.Name, v.Options.SnapshotOnRemove)
	return nil
}

// deleteInBackground snapshots (optionally) and deletes a PD off the Docker
// request path, so a slow GCE delete never trips the daemon's request timeout.
//
// DeleteDisk marks the disk with the gcepd-deleting label before doing any slow
// work, so a concurrent Create is refused and an interrupted delete is resumable
// from reconciliation — there is no in-memory bookkeeping to keep here.
func (d *Driver) deleteInBackground(name string, snapshotFirst bool) {
	d.bg.Add(1)
	go func() {
		defer d.bg.Done()
		ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
		defer cancel()
		if err := d.gce.DeleteDisk(ctx, name, snapshotFirst); err != nil {
			// The disk still carries managed-by + gcepd-deleting, so reconciliation
			// will resume the delete; surface the failure loudly for the operator.
			log.Printf("gcepd: ERROR: background delete of disk %q failed: %v "+
				"(it remains in GCE marked for deletion and will be retried at next startup)", name, err)
			return
		}
		log.Printf("gcepd: background delete of disk %q complete", name)
	}()
}

// WaitBackground blocks until in-flight background deletes finish. Intended for
// graceful shutdown / tests.
func (d *Driver) WaitBackground() { d.bg.Wait() }

// Mount attaches the disk to this VM, formats it if blank, mounts it, and
// increments the reference count. Repeated mounts of the same volume reuse the
// existing mountpoint and just bump the count.
func (d *Driver) Mount(req *volume.MountRequest) (*volume.MountResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	v, ok := d.state.Get(req.Name)
	if !ok {
		return nil, fmt.Errorf("volume %q not found", req.Name)
	}

	target := mountpointFor(req.Name)

	// Already mounted by a previous container: just bump the ref count.
	if v.RefCount > 0 {
		if _, err := d.state.IncRef(req.Name, target); err != nil {
			return nil, err
		}
		return &volume.MountResponse{Mountpoint: target}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	if err := d.gce.AttachDisk(ctx, req.Name); err != nil {
		return nil, err
	}

	device, err := d.mount.WaitForDevice(ctx, req.Name)
	if err != nil {
		// Best-effort detach so we don't leave a dangling attachment.
		_ = d.gce.DetachDisk(ctx, req.Name)
		return nil, err
	}

	if _, err := d.mount.EnsureFormatted(ctx, device, v.Options.FSType); err != nil {
		_ = d.gce.DetachDisk(ctx, req.Name)
		return nil, err
	}

	if err := d.mount.Mount(ctx, device, target); err != nil {
		_ = d.gce.DetachDisk(ctx, req.Name)
		return nil, err
	}

	if _, err := d.state.IncRef(req.Name, target); err != nil {
		// State write failed after a successful mount: unwind to a consistent
		// state rather than leaking an attachment the count doesn't know about.
		_ = d.mount.Unmount(ctx, target)
		_ = d.gce.DetachDisk(ctx, req.Name)
		return nil, fmt.Errorf("mount %q: state write failed: %w", req.Name, err)
	}

	return &volume.MountResponse{Mountpoint: target}, nil
}

// Unmount decrements the reference count and, when it reaches zero, unmounts and
// detaches the disk.
func (d *Driver) Unmount(req *volume.UnmountRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	v, ok := d.state.Get(req.Name)
	if !ok {
		return fmt.Errorf("volume %q not found", req.Name)
	}
	if v.RefCount == 0 {
		return nil // nothing mounted
	}

	n, err := d.state.DecRef(req.Name)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil // still in use by other containers
	}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()

	target := mountpointFor(req.Name)
	if err := d.mount.Unmount(ctx, target); err != nil {
		return err
	}
	return d.gce.DetachDisk(ctx, req.Name)
}

// Get returns a single volume's view.
func (d *Driver) Get(req *volume.GetRequest) (*volume.GetResponse, error) {
	v, ok := d.state.Get(req.Name)
	if !ok {
		return nil, fmt.Errorf("volume %q not found", req.Name)
	}
	return &volume.GetResponse{Volume: toAPIVolume(v)}, nil
}

// List returns all known volumes.
func (d *Driver) List() (*volume.ListResponse, error) {
	vols := d.state.List()
	out := make([]*volume.Volume, 0, len(vols))
	for _, v := range vols {
		out = append(out, toAPIVolume(v))
	}
	return &volume.ListResponse{Volumes: out}, nil
}

// Path returns the mountpoint of a volume if it is mounted.
func (d *Driver) Path(req *volume.PathRequest) (*volume.PathResponse, error) {
	v, ok := d.state.Get(req.Name)
	if !ok {
		return nil, fmt.Errorf("volume %q not found", req.Name)
	}
	return &volume.PathResponse{Mountpoint: v.Mountpoint}, nil
}

// Capabilities reports the volume scope. "local" (default) binds a volume to one
// VM. "global" (Swarm mode) advertises volumes cluster-wide so a rescheduled
// task reuses the same volume; the GCE layer then fails the disk over between
// VMs on Mount.
func (d *Driver) Capabilities() *volume.CapabilitiesResponse {
	return &volume.CapabilitiesResponse{
		Capabilities: volume.Capability{Scope: d.scope},
	}
}
