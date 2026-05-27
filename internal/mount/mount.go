// Package mount wraps the host commands the plugin needs to turn an attached
// GCE Persistent Disk into a usable mountpoint: blkid (detect existing
// filesystem), mkfs (format a blank disk), mount and umount.
//
// All external commands go through the runner interface; device/path existence
// goes through the fileSystem interface; waiting uses the clock interface. This
// keeps the decision logic (format-if-blank, argv construction, device-wait
// polling) unit-testable without root or a real disk.
package mount

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DeviceByIDPrefix is where GCE surfaces attached disks. With DeviceName set to
// the disk name at attach time, the disk appears at
// /dev/disk/by-id/google-<name>.
const DeviceByIDPrefix = "/dev/disk/by-id/google-"

// DefaultDeviceTimeout bounds how long Mounter.WaitForDevice waits for the
// kernel/udev to materialize the device node after a GCE attach.
const DefaultDeviceTimeout = 60 * time.Second

// fileSystem abstracts the few filesystem probes we do, so tests can fake them.
type fileSystem interface {
	stat(path string) error // nil if path exists
	mkdirAll(path string, p os.FileMode) error
	readMounts() (string, error) // contents of /proc/mounts
}

// clock abstracts time for the device-wait loop.
type clock interface {
	now() time.Time
	sleep(d time.Duration)
}

// Mounter performs format/mount/umount operations.
type Mounter struct {
	run runner
	fs  fileSystem
	clk clock

	deviceTimeout time.Duration
}

// New returns a Mounter backed by real syscalls and command execution.
func New() *Mounter {
	return &Mounter{
		run:           execRunner{},
		fs:            osFS{},
		clk:           realClock{},
		deviceTimeout: DefaultDeviceTimeout,
	}
}

// DevicePath returns the canonical /dev/disk/by-id path for a disk attached
// with device-name == volume name.
func DevicePath(volumeName string) string {
	return DeviceByIDPrefix + volumeName
}

// WaitForDevice blocks until the device node for the volume appears, or until
// the device timeout elapses. GCE reports the attach operation complete before
// udev has necessarily created the symlink, so we must poll.
func (m *Mounter) WaitForDevice(ctx context.Context, volumeName string) (string, error) {
	dev := DevicePath(volumeName)
	deadline := m.clk.now().Add(m.deviceTimeout)

	for {
		if err := m.fs.stat(dev); err == nil {
			return dev, nil
		}
		if m.clk.now().After(deadline) {
			return "", fmt.Errorf("device %s did not appear within %s after attach", dev, m.deviceTimeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		m.clk.sleep(500 * time.Millisecond)
	}
}

// ProbeFSType returns the filesystem type currently on the device, or "" if the
// device has no recognizable filesystem (i.e. it is blank and needs mkfs).
//
// blkid exit codes: 0 = found, 2 = nothing found (blank device). Any other
// non-zero is a real error.
func (m *Mounter) ProbeFSType(ctx context.Context, device string) (string, error) {
	// -o value -s TYPE prints just the filesystem type, nothing else.
	out, err := m.run.run(ctx, "blkid", "-p", "-o", "value", "-s", "TYPE", device)
	if err != nil {
		if exitCode(err) == 2 {
			return "", nil // blank device, not an error
		}
		return "", fmt.Errorf("blkid on %s: %w", device, err)
	}
	return strings.TrimSpace(out), nil
}

// Format creates a filesystem of the given type on the device. It must only be
// called on a device ProbeFSType reported as blank.
func (m *Mounter) Format(ctx context.Context, device, fsType string) error {
	args, err := mkfsArgs(fsType, device)
	if err != nil {
		return err
	}
	if _, err := m.run.run(ctx, args[0], args[1:]...); err != nil {
		return fmt.Errorf("format %s as %s: %w", device, fsType, err)
	}
	return nil
}

// mkfsArgs builds the mkfs command line for a supported filesystem. Both
// ext4 and xfs default to non-interactive, force-friendly invocations.
func mkfsArgs(fsType, device string) ([]string, error) {
	switch fsType {
	case "ext4":
		// -F: don't prompt when the target looks like a whole disk.
		return []string{"mkfs.ext4", "-F", device}, nil
	case "xfs":
		// -f: force overwrite if a (stale) signature is present.
		return []string{"mkfs.xfs", "-f", device}, nil
	default:
		return nil, fmt.Errorf("unsupported filesystem %q (supported: ext4, xfs)", fsType)
	}
}

// EnsureFormatted formats the device with fsType only if it has no filesystem.
// Returns the detected/created filesystem type. This is the format-if-blank
// decision the driver relies on at Mount.
func (m *Mounter) EnsureFormatted(ctx context.Context, device, fsType string) (string, error) {
	existing, err := m.ProbeFSType(ctx, device)
	if err != nil {
		return "", err
	}
	if existing != "" {
		return existing, nil // already has a filesystem; never reformat
	}
	if err := m.Format(ctx, device, fsType); err != nil {
		return "", err
	}
	return fsType, nil
}

// Mount mounts device at target, creating target if needed. It is idempotent:
// if device is already mounted at target, it returns nil.
func (m *Mounter) Mount(ctx context.Context, device, target string) error {
	mounted, err := m.IsMounted(target)
	if err != nil {
		return err
	}
	if mounted {
		return nil
	}
	if err := m.fs.mkdirAll(target, 0o755); err != nil {
		return fmt.Errorf("create mountpoint %s: %w", target, err)
	}
	if _, err := m.run.run(ctx, "mount", device, target); err != nil {
		return fmt.Errorf("mount %s at %s: %w", device, target, err)
	}
	return nil
}

// Unmount unmounts target. It is idempotent: unmounting a path that is not
// mounted returns nil.
func (m *Mounter) Unmount(ctx context.Context, target string) error {
	mounted, err := m.IsMounted(target)
	if err != nil {
		return err
	}
	if !mounted {
		return nil
	}
	if _, err := m.run.run(ctx, "umount", target); err != nil {
		return fmt.Errorf("umount %s: %w", target, err)
	}
	return nil
}

// IsMounted reports whether target is a current mountpoint, by scanning
// /proc/mounts. We match the mountpoint (second field) exactly.
func (m *Mounter) IsMounted(target string) (bool, error) {
	clean := filepath.Clean(target)
	mounts, err := m.fs.readMounts()
	if err != nil {
		return false, fmt.Errorf("read /proc/mounts: %w", err)
	}
	for _, line := range strings.Split(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if filepath.Clean(fields[1]) == clean {
			return true, nil
		}
	}
	return false, nil
}

// ---- real implementations of fileSystem / clock ----

type osFS struct{}

func (osFS) stat(path string) error                    { _, err := os.Stat(path); return err }
func (osFS) mkdirAll(path string, p os.FileMode) error { return os.MkdirAll(path, p) }
func (osFS) readMounts() (string, error) {
	b, err := os.ReadFile("/proc/mounts")
	return string(b), err
}

type realClock struct{}

func (realClock) now() time.Time        { return time.Now() }
func (realClock) sleep(d time.Duration) { time.Sleep(d) }
