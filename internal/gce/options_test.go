package gce

import "testing"

func TestValidateDiskName(t *testing.T) {
	valid := []string{"a", "vol", "my-volume", "v1", "a" + repeat("b", 62)}
	for _, n := range valid {
		if err := ValidateDiskName(n); err != nil {
			t.Errorf("ValidateDiskName(%q) = %v, want nil", n, err)
		}
	}

	invalid := []string{
		"",                    // empty
		"1abc",                // starts with digit
		"-abc",                // starts with hyphen
		"abc-",                // trailing hyphen
		"AbC",                 // uppercase
		"my_volume",           // underscore
		"a" + repeat("b", 63), // 64 chars, one over the limit
	}
	for _, n := range invalid {
		if err := ValidateDiskName(n); err == nil {
			t.Errorf("ValidateDiskName(%q) = nil, want error", n)
		}
	}
}

func TestParseDiskOptionsDefaults(t *testing.T) {
	opts, err := ParseDiskOptions(nil)
	if err != nil {
		t.Fatalf("ParseDiskOptions(nil) error = %v", err)
	}
	if opts.SizeGB != DefaultSizeGB || opts.Type != DefaultDiskType || opts.FSType != DefaultFSType {
		t.Errorf("defaults = %+v, want size=%d type=%s fs=%s",
			opts, DefaultSizeGB, DefaultDiskType, DefaultFSType)
	}
	if opts.ReclaimPolicy != ReclaimRetain {
		t.Errorf("default ReclaimPolicy = %q, want retain", opts.ReclaimPolicy)
	}
}

func TestParseDiskOptionsValid(t *testing.T) {
	opts, err := ParseDiskOptions(map[string]string{
		"size":             "50",
		"type":             "pd-ssd",
		"fs":               "xfs",
		"labels":           "team=infra,env=prod",
		"snapshotOnRemove": "true",
		"reclaimPolicy":    "delete",
	})
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if opts.SizeGB != 50 || opts.Type != "pd-ssd" || opts.FSType != "xfs" {
		t.Errorf("got %+v", opts)
	}
	if !opts.SnapshotOnRemove {
		t.Error("SnapshotOnRemove = false, want true")
	}
	if opts.ReclaimPolicy != ReclaimDelete {
		t.Errorf("ReclaimPolicy = %q, want delete", opts.ReclaimPolicy)
	}
	if opts.Labels["team"] != "infra" || opts.Labels["env"] != "prod" {
		t.Errorf("labels = %v", opts.Labels)
	}
}

func TestParseDiskOptionsErrors(t *testing.T) {
	cases := []map[string]string{
		{"size": "0"},                               // non-positive
		{"size": "abc"},                             // not a number
		{"type": "pd-unknown"},                      // bad type
		{"fs": "btrfs"},                             // unsupported fs
		{"bogus": "x"},                              // unknown key
		{"labels": "managed-by=evil"},               // reserved label
		{"labels": "noequals"},                      // malformed label
		{"sourceSnapshot": "s", "sourceImage": "i"}, // mutually exclusive
		{"snapshotOnRemove": "maybe"},               // bad bool
		{"reclaimPolicy": "keep"},                   // invalid policy
	}
	for _, raw := range cases {
		if _, err := ParseDiskOptions(raw); err == nil {
			t.Errorf("ParseDiskOptions(%v) = nil, want error", raw)
		}
	}
}

func TestEffectiveLabelsAlwaysIncludesManagedBy(t *testing.T) {
	opts, _ := ParseDiskOptions(map[string]string{"labels": "a=b"})
	got := opts.EffectiveLabels()
	if got[ManagedByLabelKey] != ManagedByLabelValue {
		t.Errorf("managed-by label = %q, want %q", got[ManagedByLabelKey], ManagedByLabelValue)
	}
	if got["a"] != "b" {
		t.Errorf("user label lost: %v", got)
	}
}

func TestMatches(t *testing.T) {
	base, _ := ParseDiskOptions(map[string]string{"size": "10", "type": "pd-balanced", "fs": "ext4", "labels": "a=b"})
	same, _ := ParseDiskOptions(map[string]string{"size": "10", "type": "pd-balanced", "fs": "ext4", "labels": "a=b"})
	diff, _ := ParseDiskOptions(map[string]string{"size": "20", "type": "pd-balanced", "fs": "ext4", "labels": "a=b"})

	if !base.Matches(same) {
		t.Error("identical options should match")
	}
	if base.Matches(diff) {
		t.Error("different size should not match")
	}
}

func repeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}
