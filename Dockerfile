# syntax=docker/dockerfile:1

# ---- builder ----------------------------------------------------------------
# Pinned to 1.25 because cloud.google.com/go/compute v1.64.0 requires that
# toolchain (see go.mod). CGO is disabled so the binary is fully static and runs
# in the minimal plugin rootfs with no libc.
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Cache module downloads independently of source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /docker-volume-gcepd \
        ./cmd/docker-volume-gcepd

# ---- plugin rootfs ----------------------------------------------------------
# The runtime filesystem for the managed plugin. It needs the userspace tools
# the mount package shells out to:
#   - e2fsprogs : mkfs.ext4, blkid (blkid also ships in util-linux/libblkid)
#   - xfsprogs  : mkfs.xfs
#   - util-linux: mount, umount, blkid
#   - ca-certificates: TLS trust for the GCE REST API
FROM alpine:3.20

RUN apk add --no-cache \
        ca-certificates \
        e2fsprogs \
        xfsprogs \
        util-linux

COPY --from=builder /docker-volume-gcepd /usr/bin/docker-volume-gcepd

# Where the plugin keeps mounts and state at runtime. Declared so the directory
# exists in the rootfs; the plugin also creates it on demand.
RUN mkdir -p /var/lib/docker-gcepd/mounts

ENTRYPOINT ["/usr/bin/docker-volume-gcepd"]
