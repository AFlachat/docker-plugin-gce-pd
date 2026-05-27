// Package state persists the plugin's local view of the volumes it manages:
// for each volume, the options it was created with, its lifecycle status, and a
// reference count of how many active mounts exist (which decides when a disk is
// detached).
//
// The store is a single JSON file written atomically under a mutex. It is the
// authoritative record of *intent*; GCE is authoritative for *reality*, and the
// two are reconciled at startup (see Reconcile).
//
// This package deliberately has no dependency on internal/gce: it defines its
// own serializable VolumeOptions, and reconciliation is driven by a plain list
// of disk names the caller fetches from GCE. This keeps the packages decoupled
// and the tests light.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DefaultPath is where the state file lives on the VM.
const DefaultPath = "/var/lib/docker-gcepd/state.json"

// Status is a volume's lifecycle stage as this plugin sees it.
type Status string

const (
	// StatusCreated: disk exists in GCE, not attached to this VM.
	StatusCreated Status = "created"
	// StatusMounted: disk is attached and mounted (refCount > 0).
	StatusMounted Status = "mounted"
)

// VolumeOptions is the serializable record of how a volume was created. It
// mirrors gce.DiskOptions but is owned by this package; the driver maps between
// the two.
type VolumeOptions struct {
	SizeGB           int64             `json:"sizeGb"`
	Type             string            `json:"type"`
	FSType           string            `json:"fsType"`
	Labels           map[string]string `json:"labels,omitempty"`
	SourceSnapshot   string            `json:"sourceSnapshot,omitempty"`
	SourceImage      string            `json:"sourceImage,omitempty"`
	SnapshotOnRemove bool              `json:"snapshotOnRemove,omitempty"`
	ReclaimPolicy    string            `json:"reclaimPolicy,omitempty"`
}

// Volume is a single managed volume's persisted record.
type Volume struct {
	Name       string        `json:"name"`
	Options    VolumeOptions `json:"options"`
	Status     Status        `json:"status"`
	Mountpoint string        `json:"mountpoint,omitempty"`
	// RefCount is the number of active container mounts. The disk is detached
	// when it drops to zero. It is not trusted across restarts (see
	// ResetRefCounts).
	RefCount int `json:"refCount"`
	// CreatedAt is set the moment the disk-create succeeds, used only for
	// observability.
	CreatedAt string `json:"createdAt,omitempty"`
}

// Store is a thread-safe, file-backed collection of Volumes.
type Store struct {
	mu   sync.Mutex
	path string
	vols map[string]*Volume
}

// Load opens (or initializes) the store at path. A missing file yields an empty
// store; a corrupt file is an error so we never silently lose track of disks.
func Load(path string) (*Store, error) {
	s := &Store{path: path, vols: map[string]*Volume{}}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state file %s: %w", path, err)
	}
	if len(b) == 0 {
		return s, nil
	}

	var vols []*Volume
	if err := json.Unmarshal(b, &vols); err != nil {
		return nil, fmt.Errorf("parse state file %s: %w", path, err)
	}
	for _, v := range vols {
		s.vols[v.Name] = v
	}
	return s, nil
}

// persist writes the store to disk atomically (write temp + rename). The caller
// must hold s.mu.
func (s *Store) persist() error {
	vols := make([]*Volume, 0, len(s.vols))
	for _, v := range s.vols {
		vols = append(vols, v)
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Name < vols[j].Name })

	b, err := json.MarshalIndent(vols, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename state into place: %w", err)
	}
	return nil
}

// Get returns a copy of the named volume's record and whether it exists.
func (s *Store) Get(name string) (Volume, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vols[name]
	if !ok {
		return Volume{}, false
	}
	return *v, true
}

// List returns copies of all volume records, sorted by name.
func (s *Store) List() []Volume {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Volume, 0, len(s.vols))
	for _, v := range s.vols {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Add inserts a new volume record and persists. It errors if the name already
// exists (callers handle idempotency by checking Get first).
func (s *Store) Add(v Volume) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vols[v.Name]; ok {
		return fmt.Errorf("volume %q already in state", v.Name)
	}
	cp := v
	s.vols[v.Name] = &cp
	return s.persist()
}

// Remove deletes a volume record and persists. Removing an absent volume is a
// no-op (idempotent).
func (s *Store) Remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.vols[name]; !ok {
		return nil
	}
	delete(s.vols, name)
	return s.persist()
}

// Update applies fn to the named volume under the lock and persists the result.
// fn may mutate the passed *Volume. Returns an error if the volume is absent.
func (s *Store) Update(name string, fn func(*Volume)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.vols[name]
	if !ok {
		return fmt.Errorf("volume %q not in state", name)
	}
	fn(v)
	return s.persist()
}

// IncRef increments a volume's reference count, sets it mounted, and records the
// mountpoint. Returns the new count.
func (s *Store) IncRef(name, mountpoint string) (int, error) {
	var n int
	err := s.Update(name, func(v *Volume) {
		v.RefCount++
		v.Status = StatusMounted
		v.Mountpoint = mountpoint
		n = v.RefCount
	})
	return n, err
}

// DecRef decrements a volume's reference count, clamping at zero. When it
// reaches zero the volume is marked created (detached) and the mountpoint
// cleared. Returns the new count.
func (s *Store) DecRef(name string) (int, error) {
	var n int
	err := s.Update(name, func(v *Volume) {
		if v.RefCount > 0 {
			v.RefCount--
		}
		n = v.RefCount
		if v.RefCount == 0 {
			v.Status = StatusCreated
			v.Mountpoint = ""
		}
	})
	return n, err
}

// ResetRefCounts zeroes every volume's reference count and marks them created.
// Called at startup: this plugin instance has no live mounts it knows about
// yet. The driver re-establishes counts for volumes still mounted on disk by
// scanning /proc/mounts and calling SetMounted.
func (s *Store) ResetRefCounts() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range s.vols {
		v.RefCount = 0
		v.Status = StatusCreated
		v.Mountpoint = ""
	}
	return s.persist()
}

// SetMounted forces a volume into the mounted state with refCount 1 at the given
// mountpoint. Used during startup reconciliation when a volume is found still
// mounted on disk.
func (s *Store) SetMounted(name, mountpoint string) error {
	return s.Update(name, func(v *Volume) {
		v.RefCount = 1
		v.Status = StatusMounted
		v.Mountpoint = mountpoint
	})
}

// Reconcile aligns the local store with the set of disk names that actually
// exist in GCE (managed by this plugin):
//
//   - a disk present in GCE but missing locally is imported (refCount 0,
//     created), so we recover from a lost/corrupt state file;
//   - a volume present locally but whose disk no longer exists in GCE is
//     dropped, so we never expose a phantom volume.
//
// importOptions supplies the options for a freshly-imported disk; if nil, the
// imported volume gets zero-value options (the driver can backfill from GCE
// later if it wishes).
func (s *Store) Reconcile(gceDiskNames []string, importOptions func(name string) VolumeOptions) (ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	gceSet := make(map[string]bool, len(gceDiskNames))
	for _, n := range gceDiskNames {
		gceSet[n] = true
	}

	var res ReconcileResult

	// Drop local volumes whose disk vanished from GCE.
	for name := range s.vols {
		if !gceSet[name] {
			delete(s.vols, name)
			res.Removed = append(res.Removed, name)
		}
	}

	// Import GCE disks missing from local state.
	for _, name := range gceDiskNames {
		if _, ok := s.vols[name]; ok {
			continue
		}
		var opts VolumeOptions
		if importOptions != nil {
			opts = importOptions(name)
		}
		s.vols[name] = &Volume{
			Name:    name,
			Options: opts,
			Status:  StatusCreated,
		}
		res.Imported = append(res.Imported, name)
	}

	sort.Strings(res.Removed)
	sort.Strings(res.Imported)
	return res, s.persist()
}

// ReconcileResult reports what Reconcile changed, for logging.
type ReconcileResult struct {
	Imported []string // disks found in GCE, added locally
	Removed  []string // local volumes whose disk no longer exists
}
