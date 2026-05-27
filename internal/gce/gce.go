// Package gce wraps the subset of the Compute Engine API the plugin needs:
// create/get/delete/list zonal Persistent Disks and attach/detach them to the
// current VM. It uses the modern cloud.google.com/go/compute/apiv1 client.
//
// Why apiv1 over google.golang.org/api/compute/v1 (legacy): the new client
// returns an *compute.Operation wrapper whose Wait() polls the right
// (zonal/regional/global) operations endpoint for us, and it is the actively
// maintained surface. The trade-off is that it is REST-only (no gRPC) and its
// clients are concrete structs; we wrap them behind small interfaces (diskAPI,
// instanceAPI) so the orchestration logic is unit-testable with fakes.
//
// Auth uses Application Default Credentials. On a GCE VM that resolves to the
// VM's service account automatically. An operator may override with a JSON key
// file via the GCEPD_KEYFILE env var (see New).
//
// v1 scope: zonal disks only. Regional (multi-zone) PDs are out of scope and
// documented as a known limitation.
package gce

import (
	"context"
	"fmt"
	"os"
	"time"

	compute "cloud.google.com/go/compute/apiv1"
	computepb "cloud.google.com/go/compute/apiv1/computepb"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
)

// iterDone aliases the iterator sentinel so the adapter reads cleanly.
var iterDone = iterator.Done

// KeyFileEnv is the env var an operator can set to point at a service-account
// JSON key, overriding the VM's default credentials.
const KeyFileEnv = "GCEPD_KEYFILE"

// operation is the minimal surface of compute.Operation we depend on, so fakes
// can return a canned operation without a real API call.
type operation interface {
	Wait(ctx context.Context, opts ...gax.CallOption) error
}

// diskAPI is the subset of *compute.DisksClient we use.
type diskAPI interface {
	Insert(ctx context.Context, req *computepb.InsertDiskRequest, opts ...gax.CallOption) (operation, error)
	Get(ctx context.Context, req *computepb.GetDiskRequest, opts ...gax.CallOption) (*computepb.Disk, error)
	Delete(ctx context.Context, req *computepb.DeleteDiskRequest, opts ...gax.CallOption) (operation, error)
	List(ctx context.Context, req *computepb.ListDisksRequest) ([]*computepb.Disk, error)
	CreateSnapshot(ctx context.Context, req *computepb.CreateSnapshotDiskRequest, opts ...gax.CallOption) (operation, error)
	Close() error
}

// instanceAPI is the subset of *compute.InstancesClient we use.
type instanceAPI interface {
	AttachDisk(ctx context.Context, req *computepb.AttachDiskInstanceRequest, opts ...gax.CallOption) (operation, error)
	DetachDisk(ctx context.Context, req *computepb.DetachDiskInstanceRequest, opts ...gax.CallOption) (operation, error)
	Get(ctx context.Context, req *computepb.GetInstanceRequest, opts ...gax.CallOption) (*computepb.Instance, error)
	Close() error
}

// Scope controls cross-VM failover behaviour.
//
//	ScopeLocal  — a disk attached to another instance is never stolen; AttachDisk
//	              errors. This is the single-VM default.
//	ScopeGlobal — Swarm mode: a disk attached to a *down* (or, after a grace
//	              window, a still-running) holder is detached and re-attached to
//	              this VM, so a rescheduled task's volume follows it.
const (
	ScopeLocal  = "local"
	ScopeGlobal = "global"
)

// DefaultForceDetachAfter is how long AttachDisk waits for a clean detach from a
// RUNNING holder before forcing, in ScopeGlobal.
const DefaultForceDetachAfter = 30 * time.Second

// Client is the high-level GCE wrapper used by the driver. It is bound to a
// single project/zone/instance (the current VM), discovered via metadata.
type Client struct {
	disks     diskAPI
	instances instanceAPI

	projectID string
	zone      string
	instance  string

	// scope is ScopeLocal or ScopeGlobal; it gates failover in AttachDisk.
	scope string
	// forceDetachAfter bounds the wait for a clean detach from a RUNNING holder
	// before forcing (ScopeGlobal only).
	forceDetachAfter time.Duration

	backoff BackoffConfig
	// opTimeout bounds how long we wait for an async operation to complete.
	opTimeout time.Duration

	// now/sleep are injectable for deterministic tests of the grace loop.
	now   func() time.Time
	sleep func(time.Duration)
}

// Config holds the immutable per-VM identity plus tunables.
type Config struct {
	ProjectID string
	Zone      string
	Instance  string

	// Scope is ScopeLocal (default) or ScopeGlobal. Empty means ScopeLocal.
	Scope string
	// ForceDetachAfter overrides DefaultForceDetachAfter when non-zero.
	ForceDetachAfter time.Duration

	// Optional tunables; zero values fall back to defaults.
	Backoff   BackoffConfig
	OpTimeout time.Duration
}

// DefaultOpTimeout is how long we wait for a disk/attach operation to finish
// before giving up. GCE disk ops are usually seconds; 2 min is generous.
const DefaultOpTimeout = 2 * time.Minute

// New builds a Client backed by the real Compute API clients, authenticated via
// ADC (or GCEPD_KEYFILE if set). Callers own the returned Client and must Close
// it.
func New(ctx context.Context, cfg Config) (*Client, error) {
	var clientOpts []option.ClientOption
	if kf := os.Getenv(KeyFileEnv); kf != "" {
		clientOpts = append(clientOpts, option.WithCredentialsFile(kf))
	}

	disks, err := compute.NewDisksRESTClient(ctx, clientOpts...)
	if err != nil {
		return nil, fmt.Errorf("create disks client: %w", err)
	}
	instances, err := compute.NewInstancesRESTClient(ctx, clientOpts...)
	if err != nil {
		_ = disks.Close()
		return nil, fmt.Errorf("create instances client: %w", err)
	}

	return newClient(cfg,
		&disksAdapter{disks},
		&instancesAdapter{instances},
	), nil
}

// newClient is the testable constructor: it takes the (possibly fake) API
// interfaces directly and applies defaults.
func newClient(cfg Config, disks diskAPI, instances instanceAPI) *Client {
	backoff := cfg.Backoff
	if backoff == (BackoffConfig{}) {
		backoff = DefaultBackoff()
	}
	opTimeout := cfg.OpTimeout
	if opTimeout == 0 {
		opTimeout = DefaultOpTimeout
	}
	scope := cfg.Scope
	if scope == "" {
		scope = ScopeLocal
	}
	forceDetachAfter := cfg.ForceDetachAfter
	if forceDetachAfter == 0 {
		forceDetachAfter = DefaultForceDetachAfter
	}
	return &Client{
		disks:            disks,
		instances:        instances,
		projectID:        cfg.ProjectID,
		zone:             cfg.Zone,
		instance:         cfg.Instance,
		scope:            scope,
		forceDetachAfter: forceDetachAfter,
		backoff:          backoff,
		opTimeout:        opTimeout,
		now:              time.Now,
		sleep:            time.Sleep,
	}
}

// Close releases the underlying API clients.
func (c *Client) Close() error {
	derr := c.disks.Close()
	ierr := c.instances.Close()
	if derr != nil {
		return derr
	}
	return ierr
}

// waitOp waits for an async operation to complete, bounded by opTimeout. The
// op.Wait call polls the appropriate operations endpoint internally.
func (c *Client) waitOp(ctx context.Context, op operation) error {
	ctx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()
	if err := op.Wait(ctx); err != nil {
		return fmt.Errorf("waiting for GCE operation: %w", err)
	}
	return nil
}

// ---- adapters: bridge the concrete REST clients to our interfaces ----
//
// The concrete methods return *compute.Operation; our interface returns the
// `operation` interface. *compute.Operation already satisfies `operation`, so
// the adapters just forward and widen the return type.

type disksAdapter struct{ c *compute.DisksClient }

func (a *disksAdapter) Insert(ctx context.Context, req *computepb.InsertDiskRequest, opts ...gax.CallOption) (operation, error) {
	return a.c.Insert(ctx, req, opts...)
}
func (a *disksAdapter) Get(ctx context.Context, req *computepb.GetDiskRequest, opts ...gax.CallOption) (*computepb.Disk, error) {
	return a.c.Get(ctx, req, opts...)
}
func (a *disksAdapter) Delete(ctx context.Context, req *computepb.DeleteDiskRequest, opts ...gax.CallOption) (operation, error) {
	return a.c.Delete(ctx, req, opts...)
}
func (a *disksAdapter) CreateSnapshot(ctx context.Context, req *computepb.CreateSnapshotDiskRequest, opts ...gax.CallOption) (operation, error) {
	return a.c.CreateSnapshot(ctx, req, opts...)
}
func (a *disksAdapter) Close() error { return a.c.Close() }

// List drains the paged iterator into a slice. We only ever expect a handful of
// managed disks, so materializing them is fine.
func (a *disksAdapter) List(ctx context.Context, req *computepb.ListDisksRequest) ([]*computepb.Disk, error) {
	var out []*computepb.Disk
	it := a.c.List(ctx, req)
	for {
		d, err := it.Next()
		if err == iterDone {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
}

type instancesAdapter struct{ c *compute.InstancesClient }

func (a *instancesAdapter) AttachDisk(ctx context.Context, req *computepb.AttachDiskInstanceRequest, opts ...gax.CallOption) (operation, error) {
	return a.c.AttachDisk(ctx, req, opts...)
}
func (a *instancesAdapter) DetachDisk(ctx context.Context, req *computepb.DetachDiskInstanceRequest, opts ...gax.CallOption) (operation, error) {
	return a.c.DetachDisk(ctx, req, opts...)
}
func (a *instancesAdapter) Get(ctx context.Context, req *computepb.GetInstanceRequest, opts ...gax.CallOption) (*computepb.Instance, error) {
	return a.c.Get(ctx, req, opts...)
}
func (a *instancesAdapter) Close() error { return a.c.Close() }
