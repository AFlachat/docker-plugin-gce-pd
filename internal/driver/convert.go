package driver

import (
	"github.com/docker/go-plugins-helpers/volume"

	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

// toStateOptions maps the GCE-side options to the serializable state form.
func toStateOptions(o gce.DiskOptions) state.VolumeOptions {
	return state.VolumeOptions{
		SizeGB:           o.SizeGB,
		Type:             o.Type,
		FSType:           o.FSType,
		Labels:           o.Labels,
		SourceSnapshot:   o.SourceSnapshot,
		SourceImage:      o.SourceImage,
		SnapshotOnRemove: o.SnapshotOnRemove,
		ReclaimPolicy:    o.ReclaimPolicy,
	}
}

// toGCEOptions is the inverse of toStateOptions.
func toGCEOptions(o state.VolumeOptions) gce.DiskOptions {
	return gce.DiskOptions{
		SizeGB:           o.SizeGB,
		Type:             o.Type,
		FSType:           o.FSType,
		Labels:           o.Labels,
		SourceSnapshot:   o.SourceSnapshot,
		SourceImage:      o.SourceImage,
		SnapshotOnRemove: o.SnapshotOnRemove,
		ReclaimPolicy:    o.ReclaimPolicy,
	}
}

// optionsMatch reports whether a stored volume's options match newly-parsed
// ones, for Create idempotency. We compare via gce.DiskOptions.Matches so the
// matching rules live in one place.
func optionsMatch(stored state.VolumeOptions, parsed gce.DiskOptions) bool {
	return toGCEOptions(stored).Matches(parsed)
}

// toAPIVolume converts an internal state.Volume into the docker API shape.
func toAPIVolume(v state.Volume) *volume.Volume {
	return &volume.Volume{
		Name:       v.Name,
		Mountpoint: v.Mountpoint,
		CreatedAt:  v.CreatedAt,
		Status: map[string]interface{}{
			"status":   string(v.Status),
			"refCount": v.RefCount,
			"sizeGb":   v.Options.SizeGB,
			"type":     v.Options.Type,
			"fsType":   v.Options.FSType,
		},
	}
}
