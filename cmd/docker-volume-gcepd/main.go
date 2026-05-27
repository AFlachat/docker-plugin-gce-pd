// Command docker-volume-gcepd is the entry point for the managed Docker volume
// plugin. It detects the GCE environment, wires up the GCE/mount/state layers,
// reconciles local state at startup, and serves the volume plugin API on the
// Unix socket Docker expects.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/docker/go-plugins-helpers/volume"

	"github.com/aflachat/docker-plugin-gce-pd/internal/driver"
	"github.com/aflachat/docker-plugin-gce-pd/internal/gce"
	"github.com/aflachat/docker-plugin-gce-pd/internal/metadata"
	"github.com/aflachat/docker-plugin-gce-pd/internal/mount"
	"github.com/aflachat/docker-plugin-gce-pd/internal/state"
)

const (
	// socketName is the plugin socket basename. The helper creates
	// /run/docker/plugins/<socketName>.sock, which must match the "socket"
	// field declared in config.json.
	socketName = "gcepd"

	// rootGID owns the socket. Docker's plugin runtime accesses it as root.
	rootGID = 0

	// startupTimeout bounds metadata discovery + reconciliation so a broken
	// environment fails fast instead of hanging the plugin.
	startupTimeout = 30 * time.Second

	// scopeEnv selects the volume scope: "local" (default) or "global" (Swarm).
	scopeEnv = "GCEPD_SCOPE"
	// forceDetachAfterEnv tunes the grace window before forcing a detach from a
	// running holder during failover (Go duration, e.g. "30s").
	forceDetachAfterEnv = "GCEPD_FORCE_DETACH_AFTER"
)

func main() {
	log.SetPrefix("gcepd: ")
	log.SetFlags(log.LstdFlags | log.LUTC)

	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), startupTimeout)
	defer cancel()

	// 1. Confirm we are on GCE and learn our identity. Fail clearly otherwise.
	md := metadata.New()
	if !md.OnGCE(ctx) {
		return errNotOnGCE
	}
	id, err := md.Fetch(ctx)
	if err != nil {
		return err
	}
	log.Printf("running on GCE: project=%s zone=%s instance=%s", id.ProjectID, id.Zone, id.InstanceName)

	// 2. Resolve scope + failover tunables from the environment.
	scope, err := resolveScope()
	if err != nil {
		return err
	}
	forceDetachAfter, err := resolveForceDetachAfter()
	if err != nil {
		return err
	}
	log.Printf("scope=%s forceDetachAfter=%s", scope, forceDetachAfter)

	// 3. Build the GCE client (ADC, or GCEPD_KEYFILE override).
	gceClient, err := gce.New(context.Background(), gce.Config{
		ProjectID:        id.ProjectID,
		Zone:             id.Zone,
		Instance:         id.InstanceName,
		Scope:            scope,
		ForceDetachAfter: forceDetachAfter,
	})
	if err != nil {
		return err
	}
	defer gceClient.Close()

	// 4. Load persistent state.
	store, err := state.Load(state.DefaultPath)
	if err != nil {
		return err
	}

	// 5. Assemble the driver and reconcile with reality before serving.
	d := driver.New(gceClient, mount.New(), store, scope)
	if err := d.Reconcile(ctx); err != nil {
		// Reconciliation talks to GCE; if it fails we refuse to serve, because
		// operating without an accurate disk inventory risks data loss.
		return err
	}

	// 6. Serve the volume plugin API. ServeUnix blocks until the socket closes.
	h := volume.NewHandler(d)
	log.Printf("serving on /run/docker/plugins/%s.sock", socketName)
	err = h.ServeUnix(socketName, rootGID)

	// Best-effort: let in-flight background deletes finish before exiting. Any
	// that don't are recovered by reconciliation on next startup.
	d.WaitBackground()
	return err
}

// resolveScope reads GCEPD_SCOPE, defaulting to local and rejecting unknowns.
func resolveScope() (string, error) {
	switch v := os.Getenv(scopeEnv); v {
	case "", gce.ScopeLocal:
		return gce.ScopeLocal, nil
	case gce.ScopeGlobal:
		return gce.ScopeGlobal, nil
	default:
		return "", fmt.Errorf("invalid %s=%q: expected %q or %q", scopeEnv, v, gce.ScopeLocal, gce.ScopeGlobal)
	}
}

// resolveForceDetachAfter reads GCEPD_FORCE_DETACH_AFTER as a Go duration,
// defaulting to gce.DefaultForceDetachAfter.
func resolveForceDetachAfter() (time.Duration, error) {
	v := os.Getenv(forceDetachAfterEnv)
	if v == "" {
		return gce.DefaultForceDetachAfter, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid %s=%q: expected a Go duration like 30s or 1m", forceDetachAfterEnv, v)
	}
	return d, nil
}

// errNotOnGCE carries an actionable message for the most common misconfiguration.
var errNotOnGCE = notOnGCEError{}

type notOnGCEError struct{}

func (notOnGCEError) Error() string {
	return "metadata server unreachable: this plugin must run on a GCE VM " +
		"(could not reach metadata.google.internal). If you are on GCE, check " +
		"the VM's network and that the metadata server is enabled."
}
