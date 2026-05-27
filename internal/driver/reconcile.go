package driver

import (
	"context"
	"log"

	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

// Reconcile aligns local state with reality at startup. It runs in two stages:
//
//  1. GCE reconciliation — list disks tagged managed-by=docker-gcepd, import any
//     missing from local state, drop any local volume whose disk is gone.
//  2. Mount reconciliation — reset all ref counts to zero, then for every volume
//     still mounted at its expected mountpoint (a mount that survived a plugin
//     restart), restore refCount to 1 so a later Unmount detaches it correctly.
//
// Errors in reconciliation are logged but, where safe, not fatal: the plugin
// should still come up and serve. A failure to even list GCE disks IS returned,
// since operating blind risks data loss.
func (d *Driver) Reconcile(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	disks, err := d.gce.ListManagedDisks(ctx)
	if err != nil {
		return err
	}

	names := make([]string, 0, len(disks))
	// Index disk info so imported volumes get real options (size/type) rather
	// than zero values.
	byName := make(map[string]state.VolumeOptions, len(disks))
	for _, dk := range disks {
		// A disk marked deleting had an interrupted delete. Don't import it as an
		// available volume; resume its deletion in the background instead.
		if dk.Deleting() {
			log.Printf("gcepd: reconcile: resuming interrupted delete of disk %q", dk.Name)
			d.deleteInBackground(dk.Name, false) // snapshot (if any) was taken pre-interruption
			continue
		}
		names = append(names, dk.Name)
		byName[dk.Name] = state.VolumeOptions{
			SizeGB: dk.SizeGB,
			Type:   dk.Type,
			// FSType is unknown from GCE metadata; default to ext4 so a later
			// Mount probes the real filesystem and never reformats.
			FSType: "ext4",
			Labels: dk.Labels,
			// A re-imported disk defaults to retain, so a subsequent
			// `docker volume rm` doesn't surprise-delete data we just recovered.
			ReclaimPolicy: gce.ReclaimRetain,
		}
	}

	res, err := d.state.Reconcile(names, func(name string) state.VolumeOptions {
		return byName[name]
	})
	if err != nil {
		return err
	}
	if len(res.Imported) > 0 {
		log.Printf("gcepd: reconcile imported disks not in local state: %v", res.Imported)
	}
	if len(res.Removed) > 0 {
		log.Printf("gcepd: reconcile removed phantom volumes (disk gone from GCE): %v", res.Removed)
	}

	// Stage 2: reset ref counts, then re-establish for volumes still mounted.
	if err := d.state.ResetRefCounts(); err != nil {
		return err
	}
	for _, v := range d.state.List() {
		target := mountpointFor(v.Name)
		mounted, err := d.mount.IsMounted(target)
		if err != nil {
			log.Printf("gcepd: reconcile: cannot check mount %s: %v", target, err)
			continue
		}
		if mounted {
			if err := d.state.SetMounted(v.Name, target); err != nil {
				log.Printf("gcepd: reconcile: cannot mark %s mounted: %v", v.Name, err)
				continue
			}
			log.Printf("gcepd: reconcile: volume %q still mounted at %s, refCount restored to 1", v.Name, target)
		}
	}

	return nil
}
