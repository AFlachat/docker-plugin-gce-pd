package gce

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ManagedByLabelKey / ManagedByLabelValue tag every disk this plugin creates so
// we can list and reconcile only our own disks, never touching foreign ones.
const (
	ManagedByLabelKey   = "managed-by"
	ManagedByLabelValue = "docker-gcepd"
)

// DeletingLabelKey marks a disk whose deletion is in progress. It is set before
// the (possibly slow, background) snapshot+delete so that a concurrent Create is
// refused and, if the plugin restarts mid-delete, reconciliation can resume the
// deletion rather than re-import the disk as an available volume. GCE label
// values must match [a-z0-9_-]*, so we use "true".
const DeletingLabelKey = "gcepd-deleting"

// GCE resource naming rules for disks: RFC1035-ish.
//   - 1-63 chars
//   - lowercase letter first
//   - lowercase letters, digits, hyphens
//   - no trailing hyphen
//
// https://cloud.google.com/compute/docs/naming-resources
var diskNameRE = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

const maxDiskNameLen = 63

// ValidateDiskName checks a volume name against GCE's naming constraints and
// returns a descriptive error if it is unusable as a disk name.
func ValidateDiskName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("volume name is empty")
	case len(name) > maxDiskNameLen:
		return fmt.Errorf("volume name %q is %d chars, GCE allows at most %d",
			name, len(name), maxDiskNameLen)
	case !diskNameRE.MatchString(name):
		return fmt.Errorf("volume name %q is invalid: must match %s "+
			"(lowercase letter first, then lowercase letters/digits/hyphens, no trailing hyphen)",
			name, diskNameRE.String())
	}
	return nil
}

// ReclaimPolicy decides what `docker volume rm` does to the backing PD.
//
//	ReclaimRetain — keep the PD in GCE (still labelled managed-by); only drop the
//	                local record. Data survives and the disk is re-imported at the
//	                next startup. This is the default, to avoid accidental loss.
//	ReclaimDelete — delete the PD from GCE (the previous behaviour).
const (
	ReclaimRetain = "retain"
	ReclaimDelete = "delete"
)

// Default disk parameters when the user omits them.
const (
	DefaultSizeGB        = 10
	DefaultDiskType      = "pd-balanced"
	DefaultFSType        = "ext4"
	DefaultReclaimPolicy = ReclaimRetain
)

// DiskOptions is the parsed, validated form of the `--opt key=value` flags
// passed to `docker volume create`. It is what we persist in local state and
// what drives both the GCE Insert request and the later mkfs/mount.
type DiskOptions struct {
	SizeGB           int64             // disk size in GiB
	Type             string            // pd-standard | pd-balanced | pd-ssd | pd-extreme
	FSType           string            // filesystem to format with if disk is blank
	Labels           map[string]string // user labels, merged with managed-by
	SourceSnapshot   string            // optional: create from snapshot
	SourceImage      string            // optional: create from image
	SnapshotOnRemove bool              // opt-in: snapshot the disk before deleting it
	ReclaimPolicy    string            // retain (default) | delete
}

// knownDiskTypes is used only to warn/validate; GCE is the source of truth, but
// catching obvious typos early gives a far better error than a 400 from the API.
var knownDiskTypes = map[string]bool{
	"pd-standard": true,
	"pd-balanced": true,
	"pd-ssd":      true,
	"pd-extreme":  true,
}

// validFSTypes are the filesystems we know how to mkfs in internal/mount.
var validFSTypes = map[string]bool{
	"ext4": true,
	"xfs":  true,
}

// ParseDiskOptions turns the raw docker option map into a DiskOptions, applying
// defaults and validating values. Unknown keys are rejected so that a typo like
// `--opt sizeGB=50` (wrong key) fails loudly instead of silently using 10 GiB.
func ParseDiskOptions(raw map[string]string) (DiskOptions, error) {
	opts := DiskOptions{
		SizeGB:        DefaultSizeGB,
		Type:          DefaultDiskType,
		FSType:        DefaultFSType,
		Labels:        map[string]string{},
		ReclaimPolicy: DefaultReclaimPolicy,
	}

	for k, v := range raw {
		switch strings.ToLower(k) {
		case "size":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				return DiskOptions{}, fmt.Errorf("invalid size %q: must be a positive integer (GiB)", v)
			}
			opts.SizeGB = n
		case "type":
			if !knownDiskTypes[v] {
				return DiskOptions{}, fmt.Errorf("invalid type %q: expected one of pd-standard, pd-balanced, pd-ssd, pd-extreme", v)
			}
			opts.Type = v
		case "fs":
			if !validFSTypes[v] {
				return DiskOptions{}, fmt.Errorf("invalid fs %q: supported filesystems are ext4, xfs", v)
			}
			opts.FSType = v
		case "labels":
			lbls, err := parseLabels(v)
			if err != nil {
				return DiskOptions{}, err
			}
			for lk, lv := range lbls {
				opts.Labels[lk] = lv
			}
		case "sourcesnapshot":
			opts.SourceSnapshot = v
		case "sourceimage":
			opts.SourceImage = v
		case "snapshotonremove":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return DiskOptions{}, fmt.Errorf("invalid snapshotOnRemove %q: expected true or false", v)
			}
			opts.SnapshotOnRemove = b
		case "reclaimpolicy":
			switch strings.ToLower(v) {
			case ReclaimRetain, ReclaimDelete:
				opts.ReclaimPolicy = strings.ToLower(v)
			default:
				return DiskOptions{}, fmt.Errorf("invalid reclaimPolicy %q: expected retain or delete", v)
			}
		default:
			return DiskOptions{}, fmt.Errorf("unknown option %q (supported: size, type, fs, labels, sourceSnapshot, sourceImage, snapshotOnRemove, reclaimPolicy)", k)
		}
	}

	if opts.SourceSnapshot != "" && opts.SourceImage != "" {
		return DiskOptions{}, fmt.Errorf("sourceSnapshot and sourceImage are mutually exclusive")
	}

	return opts, nil
}

// parseLabels parses a "k1=v1,k2=v2" string into a map. The managed-by label is
// reserved and may not be set by the user.
func parseLabels(s string) (map[string]string, error) {
	out := map[string]string{}
	if strings.TrimSpace(s) == "" {
		return out, nil
	}
	for _, pair := range strings.Split(s, ",") {
		kv := strings.SplitN(pair, "=", 2)
		if len(kv) != 2 || kv[0] == "" {
			return nil, fmt.Errorf("invalid label %q: expected key=value pairs separated by commas", pair)
		}
		key := strings.TrimSpace(kv[0])
		if key == ManagedByLabelKey {
			return nil, fmt.Errorf("label %q is reserved by the plugin", ManagedByLabelKey)
		}
		out[key] = strings.TrimSpace(kv[1])
	}
	return out, nil
}

// EffectiveLabels returns the user labels merged with the mandatory managed-by
// label. The managed-by label always wins.
func (o DiskOptions) EffectiveLabels() map[string]string {
	labels := make(map[string]string, len(o.Labels)+1)
	for k, v := range o.Labels {
		labels[k] = v
	}
	labels[ManagedByLabelKey] = ManagedByLabelValue
	return labels
}

// Matches reports whether two DiskOptions describe the same disk, used for
// Create idempotency: a second Create with identical options is a no-op, a
// second Create with *different* options is an error.
func (o DiskOptions) Matches(other DiskOptions) bool {
	if o.SizeGB != other.SizeGB || o.Type != other.Type || o.FSType != other.FSType {
		return false
	}
	if o.SourceSnapshot != other.SourceSnapshot || o.SourceImage != other.SourceImage {
		return false
	}
	if o.ReclaimPolicy != other.ReclaimPolicy {
		return false
	}
	if len(o.Labels) != len(other.Labels) {
		return false
	}
	for k, v := range o.Labels {
		if other.Labels[k] != v {
			return false
		}
	}
	return true
}
