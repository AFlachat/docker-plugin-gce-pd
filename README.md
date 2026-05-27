# docker-plugin-gce-pd

Docker managed volume plugin that backs Docker volumes with Google Compute
Engine zonal Persistent Disks. On a GCE VM it creates the PD, attaches it,
formats it if blank, mounts it, and exposes it as a Docker volume — then
unmounts, detaches, and deletes it as volumes go away.

```bash
docker volume create --driver gcepd \
  --opt size=50 --opt type=pd-balanced --opt fs=ext4 \
  my-volume

docker run --rm -v my-volume:/data alpine sh -c 'echo hi > /data/hi.txt'
```

Written in Go on `github.com/docker/go-plugins-helpers/volume` and the
`cloud.google.com/go/compute/apiv1` SDK.

---

## How it works

| Docker action | What the plugin does |
|---|---|
| `volume create` | Creates a zonal PD in the VM's zone, tagged `managed-by=docker-gcepd`. Does **not** attach yet. |
| `docker run -v` (mount) | Attaches the PD to the VM, waits for `/dev/disk/by-id/google-<name>`, formats it **only if blank**, mounts it under `/var/lib/docker-gcepd/mounts/<name>`. |
| second container, same volume | Reuses the mount, bumps a reference count. No re-attach. |
| container stop (unmount) | Decrements the ref count; when it hits zero, unmounts and detaches the PD. |
| `volume rm` | Refuses if still attached/in use; otherwise deletes the PD (optionally snapshotting first). |

State (volume name, options, status, ref count) is persisted to
`/var/lib/docker-gcepd/state.json` and reconciled against GCE at startup.

---

## Prerequisites

### 1. A GCE VM

The plugin **only runs on a GCE VM**. It discovers its project, zone, and
instance name from the metadata server and fails fast with a clear message
elsewhere. Only **zonal** Persistent Disks are supported (see
[Limitations](#limitations)).

### 2. IAM permissions for the VM's service account

The plugin authenticates with **Application Default Credentials** — by default
the VM's attached service account. Grant it at minimum these permissions
(a custom role is cleaner than `roles/compute.instanceAdmin`):

```
compute.disks.create
compute.disks.delete
compute.disks.get
compute.disks.list
compute.disks.createSnapshot     # only if you use snapshotOnRemove
compute.instances.attachDisk
compute.instances.detachDisk
compute.instances.get
compute.zoneOperations.get        # to poll async operations
```

Create the role and bind it:

```bash
gcloud iam roles create gcepdPlugin --project "$PROJECT" \
  --title "docker-volume-gcepd" \
  --permissions=compute.disks.create,compute.disks.delete,compute.disks.get,compute.disks.list,compute.disks.createSnapshot,compute.instances.attachDisk,compute.instances.detachDisk,compute.instances.get,compute.zoneOperations.get

gcloud projects add-iam-policy-binding "$PROJECT" \
  --member="serviceAccount:$SA_EMAIL" \
  --role="projects/$PROJECT/roles/gcepdPlugin"
```

### 3. API access scope

If your VM uses legacy access scopes (instead of full IAM), it needs the
`https://www.googleapis.com/auth/compute` scope. Modern VMs using
"Allow full access to all Cloud APIs" or IAM-only access are fine.

### 4. Credential override (optional)

To use a specific service-account key instead of the VM's identity, set
`GCEPD_KEYFILE` to a JSON key path **inside the plugin rootfs** and bind it in:

```bash
docker plugin set gcepd GCEPD_KEYFILE=/run/secrets/gcepd-sa.json
```

---

## Installation

Managed plugins are single-architecture, so images are published per arch and
per release under a `<version>-<arch>` tag. Pick the one matching your VM
(`amd64` for most GCE VMs, `arm64` for Tau T2A) and a released version:

```bash
docker plugin install ghcr.io/aflachat/docker-plugin-gce-pd:1.0.0-amd64 --alias gcepd
docker plugin enable gcepd
```

See the repository's released tags for available versions.

`--alias gcepd` lets you use the short driver name `--driver gcepd`. Verify:

```bash
docker plugin ls
```

The plugin requests `CAP_SYS_ADMIN`, access to all devices, and a propagated
mount — Docker will show these for confirmation on install (see
[Security](#security)).

---

## Usage

### Create a volume

```bash
docker volume create --driver gcepd \
  --opt size=100 \
  --opt type=pd-ssd \
  --opt fs=xfs \
  --opt labels=team=data,env=prod \
  data-volume
```

### Use it

```bash
docker run --rm -v data-volume:/data alpine df -h /data
```

### Remove it

```bash
docker volume rm data-volume
```

What happens to the PD depends on the volume's `reclaimPolicy` (see options):
with the default `retain` the disk is **kept** in GCE and only forgotten
locally; with `delete` the disk is deleted.

### Options

All options are passed via `--opt key=value` to `docker volume create`.

| Option | Default | Description |
|---|---|---|
| `size` | `10` | Disk size in **GiB**. Positive integer. |
| `type` | `pd-balanced` | Disk type: `pd-standard`, `pd-balanced`, `pd-ssd`, `pd-extreme`. |
| `fs` | `ext4` | Filesystem for a blank disk: `ext4` or `xfs`. Existing filesystems are never reformatted. |
| `reclaimPolicy` | `retain` | What `docker volume rm` does to the PD: `retain` (keep it in GCE, just forget it locally) or `delete` (delete the PD). |
| `labels` | — | Comma-separated `k=v` labels applied to the PD, e.g. `team=data,env=prod`. The `managed-by` key is reserved. |
| `sourceSnapshot` | — | Create the disk from a snapshot (mutually exclusive with `sourceImage`). |
| `sourceImage` | — | Create the disk from an image (mutually exclusive with `sourceSnapshot`). |
| `snapshotOnRemove` | `false` | With `reclaimPolicy=delete`, take a snapshot of the PD before deleting it. |

Unknown options are rejected so typos fail loudly rather than silently using a
default.

#### Reclaim policy

`reclaimPolicy` defaults to **`retain`** so `docker volume rm` never destroys
data by accident. A retained PD keeps its `managed-by=docker-gcepd` label, so the
plugin re-imports it at startup and a `docker volume create` of the same name
reuses it (and its data). To actually delete the disk, create the volume with
`--opt reclaimPolicy=delete` (optionally with `--opt snapshotOnRemove=true` to
snapshot first).

### docker-compose

```yaml
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data

volumes:
  pgdata:
    driver: gcepd
    driver_opts:
      size: "50"
      type: pd-ssd
      fs: ext4
      snapshotOnRemove: "true"
```

> Note: `driver_opts` values must be quoted strings in compose.

---

## Docker Swarm (same-zone failover)

By default the plugin is `local`-scoped: a volume is bound to one VM for life,
and the plugin refuses to attach a disk that GCE reports as attached elsewhere.

For Swarm, enable `global` scope so a volume can **follow a rescheduled task** to
another VM **in the same zone**. When a task moves from VM-A to VM-B, B's plugin
detaches the disk from A and attaches it to B.

Enable it per node (set on the plugin, then it sticks):

```bash
docker plugin install ghcr.io/aflachat/docker-plugin-gce-pd:1.0.0-amd64 --alias gcepd --disable
docker plugin set gcepd GCEPD_SCOPE=global
docker plugin enable gcepd
```

Keep a service's tasks in a single zone, since the disk is zonal. Label your
nodes by zone and constrain placement:

```bash
docker node update --label-add zone=europe-west1-b <node>
```

```yaml
services:
  db:
    image: postgres:16
    volumes:
      - pgdata:/var/lib/postgresql/data
    deploy:
      replicas: 1
      placement:
        constraints:
          - node.labels.zone == europe-west1-b

volumes:
  pgdata:
    driver: gcepd
    driver_opts:
      size: "50"
      type: pd-ssd
```

### How failover decides to detach

When B wants a disk still held by A, the plugin reads the holder's state from
GCE:

- **Holder down** (`TERMINATED` / `STOPPED` / `STOPPING` / `SUSPENDED` / no
  longer exists) → it cannot be writing, so the disk is detached immediately and
  attached to B.
- **Holder still `RUNNING`** → the plugin requests a clean detach and waits up to
  `GCEPD_FORCE_DETACH_AFTER` (default `30s`) for the disk to be released. If it
  releases, B attaches it. If not, the plugin **forces** the detach and proceeds.

> ⚠️ **Data-safety warning.** A zonal PD is single-writer. The fencing above is
> best-effort: if a holder is `RUNNING` but wedged and *still writing* when the
> grace window expires, forcing the detach and mounting on B can corrupt the
> filesystem. Use `global` scope for workloads where a task is reliably stopped
> (or its VM is gone) before being rescheduled, and size
> `GCEPD_FORCE_DETACH_AFTER` to your environment. This is inherent to block
> storage without a real fencing agent — there is no distributed lock.

---

## Security

The plugin's `config.json` requests:

- **`CAP_SYS_ADMIN`** — required to `mount`/`umount` filesystems.
- **`allowAllDevices: true`** — the attached PD appears as a block device under
  `/dev/disk/by-id/google-<name>` whose major/minor numbers aren't known ahead of
  time, so a static device allowlist is insufficient.
- **Bind mount of host `/dev`** with `rshared` propagation, so newly attached
  devices become visible inside the plugin.
- **Propagated mount** of `/var/lib/docker-gcepd/mounts`, so volumes mounted by
  the plugin propagate back to the host and into target containers.
- **Host networking**, to reach the metadata server and the Compute API.

These are inherent to a volume plugin that formats and mounts real block
devices. Review them on install.

---

## Troubleshooting

**View plugin logs.** Managed-plugin output goes to the Docker daemon journal:

```bash
journalctl -u docker | grep gcepd
# or, for the plugin process specifically:
journalctl -u docker -f
```

**"metadata server unreachable: this plugin must run on a GCE VM".**
You're not on GCE, or the metadata server is blocked. The plugin refuses to
enable off-GCE by design.

**Plugin won't enable / "permission denied" in logs.**
The VM's service account is missing IAM permissions or the compute scope. See
[Prerequisites](#prerequisites). Check with:

```bash
gcloud compute instances describe "$(hostname)" --zone "$ZONE" \
  --format='value(serviceAccounts[].scopes)'
```

**Plugin shows `disabled` after a reboot.**
Managed plugins don't auto-enable on boot unless configured. Re-enable it (and
consider a systemd drop-in or startup script):

```bash
docker plugin enable gcepd
```

On enable, the plugin reconciles: it re-imports any `managed-by=docker-gcepd`
disks it finds, drops phantom entries whose disk is gone, and restores ref
counts for volumes still mounted on disk.

**`volume rm` fails with "still attached" / "in use".**
A container still references it, or a detach hasn't completed. Stop the
container(s); the plugin detaches when the last mount goes away.

**A disk is left attached after a crash.**
Reconciliation handles state, but a hard crash mid-attach can leave a disk
attached. Detach manually:

```bash
gcloud compute instances detach-disk "$(hostname)" --disk <name> --zone "$ZONE"
```

**Device never appears after attach.**
The plugin polls `/dev/disk/by-id/google-<name>` for up to 60s. If it times out,
check that the `/dev` bind mount and `rshared` propagation survived (some host
configurations reset propagation); `docker plugin disable/enable gcepd` usually
restores it.

---

## Limitations

- **Zonal PDs only.** Regional (multi-zone replicated) Persistent Disks are not
  supported. A volume lives in one zone.
- **One writer at a time.** A zonal PD attaches read-write to a single VM.
  Multi-attach / read-only fan-out is not supported. In `global` scope the disk
  moves between VMs on failover, but never has two writers (subject to the
  fencing caveat above).
- **Same-zone failover only.** `global` scope moves a volume between VMs in the
  *same* zone. There is no cross-zone migration; keep a service's tasks pinned to
  one zone with placement constraints.
- **GCE naming rules.** Volume names must be valid disk names:
  `^[a-z]([-a-z0-9]*[a-z0-9])?$`, max 63 characters (lowercase letter first,
  then lowercase letters/digits/hyphens, no trailing hyphen).
- **Filesystems.** Only `ext4` and `xfs` are formatted by the plugin. An
  imported disk with another filesystem will still mount (the plugin probes
  rather than reformats), but the plugin won't create other types.

---

## Development

```bash
make build        # static binary into ./bin
make test         # unit tests (race + coverage); no GCE needed
make lint         # go vet + gofmt check
make plugin-create REGISTRY=ghcr.io/aflachat TAG=dev   # package the managed plugin
make plugin-push   REGISTRY=ghcr.io/aflachat TAG=dev   # push to the registry
```

Packaging (`build` / `rootfs` / `plugin-create` / `plugin-push`) runs anywhere
with a Docker daemon and never touches GCE. Only `plugin-enable` and actually
running the plugin require a GCE VM.

### Integration test

A full create / mount / write / unmount / remove cycle against real GCE lives
behind the `integration` build tag and is skipped everywhere else. On a GCE VM
with the IAM permissions above:

```bash
sudo -E go test -tags=integration -run TestIntegrationFullCycle \
  -v -timeout 10m ./internal/driver/
```

It creates and deletes a disk named `gcepd-itest-<pid>`. If interrupted, delete
it manually:

```bash
gcloud compute disks delete gcepd-itest-<pid> --zone "$ZONE"
```

---

## Architecture

```
cmd/docker-volume-gcepd/   entry point: detect GCE, wire up, reconcile, serve
internal/metadata/         GCE metadata-server client (project/zone/instance)
internal/gce/              Compute API wrapper (create/get/delete/attach/detach)
internal/mount/            blkid / mkfs / mount / umount wrappers
internal/state/            JSON state persistence + ref counting + reconciliation
internal/driver/           volume.Driver implementation orchestrating the above
```

GCE API calls retry with exponential backoff and jitter; async operations are
awaited with a configurable timeout.

---

## License

MIT — see [LICENSE](LICENSE).
