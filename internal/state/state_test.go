package state

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func tmpStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return s
}

func sampleVol(name string) Volume {
	return Volume{
		Name:    name,
		Options: VolumeOptions{SizeGB: 10, Type: "pd-balanced", FSType: "ext4"},
		Status:  StatusCreated,
	}
}

func TestAddGetListRemove(t *testing.T) {
	s := tmpStore(t)
	if err := s.Add(sampleVol("vol1")); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(sampleVol("vol2")); err != nil {
		t.Fatal(err)
	}

	got, ok := s.Get("vol1")
	if !ok || got.Options.SizeGB != 10 {
		t.Errorf("Get(vol1) = %+v, %v", got, ok)
	}

	list := s.List()
	if len(list) != 2 || list[0].Name != "vol1" || list[1].Name != "vol2" {
		t.Errorf("List = %+v", list)
	}

	if err := s.Remove("vol1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("vol1"); ok {
		t.Error("vol1 should be gone")
	}
	// idempotent remove
	if err := s.Remove("vol1"); err != nil {
		t.Errorf("removing absent volume should be nil, got %v", err)
	}
}

func TestAddDuplicateFails(t *testing.T) {
	s := tmpStore(t)
	_ = s.Add(sampleVol("vol1"))
	if err := s.Add(sampleVol("vol1")); err == nil {
		t.Error("duplicate Add should fail")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s1, _ := Load(path)
	v := sampleVol("vol1")
	v.RefCount = 3
	v.Status = StatusMounted
	v.Mountpoint = "/mnt/vol1"
	if err := s1.Add(v); err != nil {
		t.Fatal(err)
	}

	// Reload from disk; data must survive.
	s2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Get("vol1")
	if !ok {
		t.Fatal("vol1 missing after reload")
	}
	if got.RefCount != 3 || got.Status != StatusMounted || got.Mountpoint != "/mnt/vol1" {
		t.Errorf("reloaded = %+v", got)
	}
	if !reflect.DeepEqual(got.Options, v.Options) {
		t.Errorf("options not preserved: %+v vs %+v", got.Options, v.Options)
	}
}

func TestLoadCorruptFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("corrupt state file should error, not silently reset")
	}
}

func TestIncDecRef(t *testing.T) {
	s := tmpStore(t)
	_ = s.Add(sampleVol("vol1"))

	n, _ := s.IncRef("vol1", "/mnt/vol1")
	if n != 1 {
		t.Errorf("IncRef -> %d, want 1", n)
	}
	n, _ = s.IncRef("vol1", "/mnt/vol1")
	if n != 2 {
		t.Errorf("IncRef -> %d, want 2", n)
	}
	v, _ := s.Get("vol1")
	if v.Status != StatusMounted || v.Mountpoint != "/mnt/vol1" {
		t.Errorf("after IncRef: %+v", v)
	}

	n, _ = s.DecRef("vol1")
	if n != 1 {
		t.Errorf("DecRef -> %d, want 1", n)
	}
	n, _ = s.DecRef("vol1")
	if n != 0 {
		t.Errorf("DecRef -> %d, want 0", n)
	}
	v, _ = s.Get("vol1")
	if v.Status != StatusCreated || v.Mountpoint != "" {
		t.Errorf("after refCount 0: %+v", v)
	}

	// DecRef clamps at zero.
	n, _ = s.DecRef("vol1")
	if n != 0 {
		t.Errorf("DecRef below zero -> %d, want 0", n)
	}
}

func TestResetRefCounts(t *testing.T) {
	s := tmpStore(t)
	v := sampleVol("vol1")
	v.RefCount = 5
	v.Status = StatusMounted
	v.Mountpoint = "/mnt/vol1"
	_ = s.Add(v)

	if err := s.ResetRefCounts(); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("vol1")
	if got.RefCount != 0 || got.Status != StatusCreated || got.Mountpoint != "" {
		t.Errorf("after reset: %+v", got)
	}
}

func TestReconcileImportsAndRemoves(t *testing.T) {
	s := tmpStore(t)
	// Local: vol1 (also in GCE), vol2 (phantom — gone from GCE).
	_ = s.Add(sampleVol("vol1"))
	_ = s.Add(sampleVol("vol2"))

	// GCE reality: vol1 still there, vol3 is new (state lost it).
	gceDisks := []string{"vol1", "vol3"}

	importOpts := func(name string) VolumeOptions {
		return VolumeOptions{SizeGB: 99, Type: "pd-ssd", FSType: "ext4"}
	}

	res, err := s.Reconcile(gceDisks, importOpts)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(res.Removed, []string{"vol2"}) {
		t.Errorf("Removed = %v, want [vol2]", res.Removed)
	}
	if !reflect.DeepEqual(res.Imported, []string{"vol3"}) {
		t.Errorf("Imported = %v, want [vol3]", res.Imported)
	}

	// vol2 gone, vol3 present with imported options.
	if _, ok := s.Get("vol2"); ok {
		t.Error("phantom vol2 should be removed")
	}
	v3, ok := s.Get("vol3")
	if !ok || v3.Options.SizeGB != 99 || v3.Status != StatusCreated {
		t.Errorf("imported vol3 = %+v, ok=%v", v3, ok)
	}
}

func TestReconcilePersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, _ := Load(path)
	_ = s.Add(sampleVol("vol-phantom"))

	if _, err := s.Reconcile([]string{}, nil); err != nil {
		t.Fatal(err)
	}
	// Reload: the phantom must stay gone after reconcile persisted.
	s2, _ := Load(path)
	if _, ok := s2.Get("vol-phantom"); ok {
		t.Error("reconcile did not persist removal")
	}
}
