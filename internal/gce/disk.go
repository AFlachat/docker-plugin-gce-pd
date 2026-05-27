package gce

import (
	"context"
	"errors"
	"fmt"
	"strings"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/protobuf/proto"
)

// ErrDiskNotFound is returned (wrapped) when a disk does not exist in GCE.
var ErrDiskNotFound = errors.New("disk not found")

// DiskInfo is a trimmed-down view of a GCE disk, exposing only what the driver
// reasons about. It hides the sprawling computepb.Disk proto.
type DiskInfo struct {
	Name     string
	SizeGB   int64
	Type     string // short type, e.g. "pd-balanced"
	Status   string // "READY", "CREATING", "FAILED", ...
	Users    []string
	Labels   map[string]string
	SelfLink string
}

// Attached reports whether the disk is currently attached to any instance.
// GCE exposes this as the Users list (each entry is an instance self-link).
func (d DiskInfo) Attached() bool { return len(d.Users) > 0 }

// Deleting reports whether this disk has been marked for deletion (a delete is
// in progress or was interrupted).
func (d DiskInfo) Deleting() bool { return d.Labels[DeletingLabelKey] == "true" }

// CreateDisk provisions a new zonal Persistent Disk in the client's zone,
// tagged with the managed-by label. It does NOT attach the disk (lazy attach
// happens at Mount). It waits for the create operation to complete.
func (c *Client) CreateDisk(ctx context.Context, name string, opts DiskOptions) (*DiskInfo, error) {
	if err := ValidateDiskName(name); err != nil {
		return nil, err
	}

	disk := &computepb.Disk{
		Name:   proto.String(name),
		SizeGb: proto.Int64(opts.SizeGB),
		// Type is a fully-qualified zonal resource URL.
		Type:   proto.String(c.diskTypeURL(opts.Type)),
		Labels: opts.EffectiveLabels(),
	}
	if opts.SourceSnapshot != "" {
		disk.SourceSnapshot = proto.String(opts.SourceSnapshot)
	}
	if opts.SourceImage != "" {
		disk.SourceImage = proto.String(opts.SourceImage)
	}

	req := &computepb.InsertDiskRequest{
		Project:      c.projectID,
		Zone:         c.zone,
		DiskResource: disk,
	}

	err := retry(ctx, c.backoff, func() error {
		op, err := c.disks.Insert(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
	if err != nil {
		return nil, fmt.Errorf("create disk %q: %w", name, err)
	}

	return c.GetDisk(ctx, name)
}

// GetDisk fetches a disk by name. Returns ErrDiskNotFound (wrapped) on 404.
func (c *Client) GetDisk(ctx context.Context, name string) (*DiskInfo, error) {
	req := &computepb.GetDiskRequest{
		Project: c.projectID,
		Zone:    c.zone,
		Disk:    name,
	}

	var disk *computepb.Disk
	err := retry(ctx, c.backoff, func() error {
		d, err := c.disks.Get(ctx, req)
		if err != nil {
			return err
		}
		disk = d
		return nil
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("%w: %s", ErrDiskNotFound, name)
		}
		return nil, fmt.Errorf("get disk %q: %w", name, err)
	}
	return diskInfoFromProto(disk), nil
}

// DeleteDisk deletes a disk by name, after refusing to delete one that is still
// attached (Users non-empty). If opts.SnapshotOnRemove is set, a snapshot is
// taken first. A 404 is treated as success (idempotent delete).
func (c *Client) DeleteDisk(ctx context.Context, name string, snapshotFirst bool) error {
	info, err := c.GetDisk(ctx, name)
	if err != nil {
		if errors.Is(err, ErrDiskNotFound) {
			return nil // already gone
		}
		return err
	}
	if info.Attached() {
		return fmt.Errorf("refusing to delete disk %q: still attached to %v", name, info.Users)
	}

	// Mark the disk as deleting before any slow work, so a concurrent Create is
	// refused and an interrupted delete is resumable from reconciliation.
	if !info.Deleting() {
		if err := c.markDeleting(ctx, info); err != nil {
			return fmt.Errorf("mark disk %q deleting: %w", name, err)
		}
	}

	if snapshotFirst {
		if err := c.snapshotDisk(ctx, name); err != nil {
			return fmt.Errorf("snapshot before delete of %q: %w", name, err)
		}
	}

	req := &computepb.DeleteDiskRequest{
		Project: c.projectID,
		Zone:    c.zone,
		Disk:    name,
	}
	err = retry(ctx, c.backoff, func() error {
		op, err := c.disks.Delete(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete disk %q: %w", name, err)
	}
	return nil
}

// ListManagedDisks returns every disk in the zone carrying our managed-by
// label. Used at startup to reconcile local state with GCE reality.
func (c *Client) ListManagedDisks(ctx context.Context) ([]DiskInfo, error) {
	// Server-side filter so we never even see foreign disks.
	filter := fmt.Sprintf("labels.%s=%s", ManagedByLabelKey, ManagedByLabelValue)
	req := &computepb.ListDisksRequest{
		Project: c.projectID,
		Zone:    c.zone,
		Filter:  proto.String(filter),
	}

	var disks []*computepb.Disk
	err := retry(ctx, c.backoff, func() error {
		d, err := c.disks.List(ctx, req)
		if err != nil {
			return err
		}
		disks = d
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list managed disks: %w", err)
	}

	out := make([]DiskInfo, 0, len(disks))
	for _, d := range disks {
		out = append(out, *diskInfoFromProto(d))
	}
	return out, nil
}

// snapshotDisk creates a timestamped snapshot of the disk before deletion.
func (c *Client) snapshotDisk(ctx context.Context, name string) error {
	// Snapshot names share the disk naming rules; suffix is bounded so we stay
	// under 63 chars even for long disk names.
	snapName := truncate(name, 40) + "-snap-" + nowStamp()
	req := &computepb.CreateSnapshotDiskRequest{
		Project: c.projectID,
		Zone:    c.zone,
		Disk:    name,
		SnapshotResource: &computepb.Snapshot{
			Name:   proto.String(snapName),
			Labels: map[string]string{ManagedByLabelKey: ManagedByLabelValue},
		},
	}
	return retry(ctx, c.backoff, func() error {
		op, err := c.disks.CreateSnapshot(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
}

// markDeleting sets the gcepd-deleting=true label on the disk, preserving its
// existing labels. SetLabels needs the current label fingerprint for optimistic
// locking, so we read it from the (just-fetched) DiskInfo's source via a Get.
func (c *Client) markDeleting(ctx context.Context, info *DiskInfo) error {
	// Re-Get the raw disk to obtain a fresh LabelFingerprint.
	getReq := &computepb.GetDiskRequest{Project: c.projectID, Zone: c.zone, Disk: info.Name}
	var fingerprint string
	labels := map[string]string{}
	err := retry(ctx, c.backoff, func() error {
		d, err := c.disks.Get(ctx, getReq)
		if err != nil {
			return err
		}
		fingerprint = d.GetLabelFingerprint()
		labels = map[string]string{}
		for k, v := range d.GetLabels() {
			labels[k] = v
		}
		return nil
	})
	if err != nil {
		return err
	}
	labels[DeletingLabelKey] = "true"

	req := &computepb.SetLabelsDiskRequest{
		Project:  c.projectID,
		Zone:     c.zone,
		Resource: info.Name,
		ZoneSetLabelsRequestResource: &computepb.ZoneSetLabelsRequest{
			LabelFingerprint: proto.String(fingerprint),
			Labels:           labels,
		},
	}
	return retry(ctx, c.backoff, func() error {
		op, err := c.disks.SetLabels(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
}

// diskTypeURL builds the fully-qualified zonal disk-type resource URL the API
// expects, e.g.
// projects/<p>/zones/<z>/diskTypes/pd-balanced
func (c *Client) diskTypeURL(t string) string {
	return fmt.Sprintf("projects/%s/zones/%s/diskTypes/%s", c.projectID, c.zone, t)
}

// diskInfoFromProto converts the verbose proto into our DiskInfo.
func diskInfoFromProto(d *computepb.Disk) *DiskInfo {
	return &DiskInfo{
		Name:     d.GetName(),
		SizeGB:   d.GetSizeGb(),
		Type:     lastURLSegment(d.GetType()),
		Status:   d.GetStatus(),
		Users:    d.GetUsers(),
		Labels:   d.GetLabels(),
		SelfLink: d.GetSelfLink(),
	}
}

// isNotFound detects a GCE 404 across the REST error type.
func isNotFound(err error) bool {
	var gerr *googleapi.Error
	if errors.As(err, &gerr) {
		return gerr.Code == 404
	}
	return false
}

// lastURLSegment returns the final path component of a GCE resource URL, used
// to turn type/zone self-links into short names.
func lastURLSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
