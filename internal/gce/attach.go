package gce

import (
	"context"
	"fmt"
	"log"
	"time"

	computepb "cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/protobuf/proto"
)

// AttachDisk attaches the named disk to the current VM in READ_WRITE mode and
// waits for completion. The device name is set to the disk name so the device
// surfaces predictably at /dev/disk/by-id/google-<name> on the VM.
//
// It is idempotent: if the disk is already attached to this instance, it
// returns nil without error.
//
// If the disk is attached to a *different* instance, behaviour depends on the
// client scope:
//   - ScopeLocal: error (a zonal PD is single-writer; we never steal it).
//   - ScopeGlobal: fail over. The holder is detached so the disk can follow a
//     rescheduled Swarm task. See ensureDetachedFromHolder for the fencing
//     rules that guard against double-writer corruption.
func (c *Client) AttachDisk(ctx context.Context, name string) error {
	info, err := c.GetDisk(ctx, name)
	if err != nil {
		return err
	}

	selfLink := c.instanceSelfLink()
	for _, u := range info.Users {
		if sameInstance(u, selfLink) {
			return nil // already attached here
		}
		// Attached to another instance.
		if c.scope != ScopeGlobal {
			return fmt.Errorf("disk %q is attached to another instance (%s); zonal PDs are single-writer", name, u)
		}
		if err := c.ensureDetachedFromHolder(ctx, name, u); err != nil {
			return err
		}
		// State changed; refetch so SelfLink/Users are current before attaching.
		info, err = c.GetDisk(ctx, name)
		if err != nil {
			return err
		}
		break
	}

	attached := &computepb.AttachedDisk{
		Source:     proto.String(info.SelfLink),
		DeviceName: proto.String(name),
		Mode:       proto.String("READ_WRITE"),
		Boot:       proto.Bool(false),
		AutoDelete: proto.Bool(false), // we manage the disk lifecycle ourselves
	}
	req := &computepb.AttachDiskInstanceRequest{
		Project:              c.projectID,
		Zone:                 c.zone,
		Instance:             c.instance,
		AttachedDiskResource: attached,
	}

	err = retry(ctx, c.backoff, func() error {
		op, err := c.instances.AttachDisk(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
	if err != nil {
		return fmt.Errorf("attach disk %q to %q: %w", name, c.instance, err)
	}
	return nil
}

// DetachDisk detaches the named disk from the current VM and waits for
// completion. It is idempotent: if the disk is not attached to this instance,
// it returns nil. The device name used at attach time equals the disk name.
func (c *Client) DetachDisk(ctx context.Context, name string) error {
	info, err := c.GetDisk(ctx, name)
	if err != nil {
		return err
	}

	selfLink := c.instanceSelfLink()
	attachedHere := false
	for _, u := range info.Users {
		if sameInstance(u, selfLink) {
			attachedHere = true
			break
		}
	}
	if !attachedHere {
		return nil // nothing to do
	}
	return c.detachFromInstance(ctx, name, c.instance)
}

// detachFromInstance detaches the named disk from an arbitrary instance (by
// name) and waits for completion. The device name equals the disk name (set at
// attach time). GCE has no separate "force" flag: detaching from the holder is
// the mechanism; callers decide whether to wait for a clean release first.
func (c *Client) detachFromInstance(ctx context.Context, name, instanceName string) error {
	req := &computepb.DetachDiskInstanceRequest{
		Project:    c.projectID,
		Zone:       c.zone,
		Instance:   instanceName,
		DeviceName: name,
	}
	err := retry(ctx, c.backoff, func() error {
		op, err := c.instances.DetachDisk(ctx, req)
		if err != nil {
			return err
		}
		return c.waitOp(ctx, op)
	})
	if err != nil {
		return fmt.Errorf("detach disk %q from %q: %w", name, instanceName, err)
	}
	return nil
}

// ensureDetachedFromHolder releases a disk from the instance currently holding
// it (holderRef is a disk.Users entry), so it can be attached to this VM. It is
// only ever called in ScopeGlobal.
//
// Fencing against double-writer corruption:
//   - If the holder is clearly down (TERMINATED/STOPPED/STOPPING/SUSPENDED, or
//     no longer exists), it cannot be writing → detach immediately.
//   - If the holder is RUNNING (ambiguous: could be wedged, could be writing),
//     issue a clean detach and poll until the disk is released, up to
//     forceDetachAfter. If it releases in time, great. If not, re-issue the
//     detach (force) and proceed, logging loudly — a rescheduled task must get
//     its volume even if the old VM is unresponsive.
func (c *Client) ensureDetachedFromHolder(ctx context.Context, name, holderRef string) error {
	holder := instanceNameFromRef(holderRef)

	status, err := c.instanceState(ctx, holder)
	if err != nil {
		return fmt.Errorf("failover for disk %q: cannot read state of holder %q: %w", name, holder, err)
	}

	if isHolderDown(status) {
		log.Printf("gcepd: failover: holder %q of disk %q is %q (down); detaching", holder, name, statusOrAbsent(status))
		return c.detachFromInstance(ctx, name, holder)
	}

	// Holder is up (RUNNING/PROVISIONING/STAGING/REPAIRING). Try a clean detach,
	// then wait for the disk to be released.
	log.Printf("gcepd: failover: holder %q of disk %q is %q (up); attempting clean detach (grace %s)",
		holder, name, status, c.forceDetachAfter)
	if err := c.detachFromInstance(ctx, name, holder); err != nil {
		return err
	}

	deadline := c.now().Add(c.forceDetachAfter)
	for {
		released, err := c.diskReleasedBy(ctx, name, holder)
		if err != nil {
			return err
		}
		if released {
			log.Printf("gcepd: failover: disk %q cleanly released by %q", name, holder)
			return nil
		}
		if c.now().After(deadline) {
			log.Printf("gcepd: WARNING: failover: disk %q not released by %q within %s; forcing detach. "+
				"If %q is still writing, data corruption is possible.", name, holder, c.forceDetachAfter, holder)
			return c.detachFromInstance(ctx, name, holder)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		c.sleep(time.Second)
	}
}

// instanceState returns the holder instance's GCE status (e.g. "RUNNING"). A
// missing instance yields "" with no error — it cannot be writing.
func (c *Client) instanceState(ctx context.Context, instanceName string) (string, error) {
	req := &computepb.GetInstanceRequest{
		Project:  c.projectID,
		Zone:     c.zone,
		Instance: instanceName,
	}
	var status string
	err := retry(ctx, c.backoff, func() error {
		inst, err := c.instances.Get(ctx, req)
		if err != nil {
			return err
		}
		status = inst.GetStatus()
		return nil
	})
	if err != nil {
		if isNotFound(err) {
			return "", nil // instance gone
		}
		return "", err
	}
	return status, nil
}

// diskReleasedBy reports whether the named disk is no longer attached to holder.
func (c *Client) diskReleasedBy(ctx context.Context, name, holder string) (bool, error) {
	info, err := c.GetDisk(ctx, name)
	if err != nil {
		return false, err
	}
	for _, u := range info.Users {
		if sameInstance(u, "projects/"+c.projectID+"/zones/"+c.zone+"/instances/"+holder) {
			return false, nil
		}
	}
	return true, nil
}

// isHolderDown reports whether a holder in the given GCE status cannot be
// writing to the disk, making an immediate detach safe. An empty status means
// the instance no longer exists.
func isHolderDown(status string) bool {
	switch status {
	case "", "TERMINATED", "STOPPED", "STOPPING", "SUSPENDED", "SUSPENDING":
		return true
	default:
		return false
	}
}

func statusOrAbsent(status string) string {
	if status == "" {
		return "absent"
	}
	return status
}

// instanceNameFromRef extracts the short instance name from a disk.Users entry,
// which is a full or partial instances URL ending in .../instances/<name>.
func instanceNameFromRef(ref string) string {
	p := trailingInstancePath(ref) // projects/.../instances/<name>
	return lastURLSegment(p)
}

// instanceSelfLink builds this VM's self-link, which is how GCE reports disk
// users. We compare against it to decide attach/detach idempotency.
func (c *Client) instanceSelfLink() string {
	return fmt.Sprintf("projects/%s/zones/%s/instances/%s", c.projectID, c.zone, c.instance)
}

// sameInstance compares two instance references that may be full URLs or
// partial paths, by matching on their trailing project/zone/instance segments.
func sameInstance(a, b string) bool {
	return trailingInstancePath(a) == trailingInstancePath(b)
}

// trailingInstancePath reduces a reference to "projects/.../instances/<name>"
// form for comparison, tolerating the https://...googleapis.com/... prefix that
// GCE puts on Users entries.
func trailingInstancePath(s string) string {
	const marker = "/projects/"
	if i := lastIndex(s, marker); i >= 0 {
		return s[i+1:]
	}
	if hasPrefix(s, "projects/") {
		return s
	}
	return s
}

// nowStamp returns a compact UTC timestamp suitable for resource name suffixes,
// e.g. 20060102t150405.
func nowStamp() string {
	return time.Now().UTC().Format("20060102t150405")
}

// small string helpers (kept local to avoid importing strings twice across
// files for trivial ops; behaviour mirrors strings.LastIndex / HasPrefix).
func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
func hasPrefix(s, p string) bool {
	return len(s) >= len(p) && s[:len(p)] == p
}
