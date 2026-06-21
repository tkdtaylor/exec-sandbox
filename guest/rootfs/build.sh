#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# build.sh — BUILD-TIME tooling (NOT a runtime dependency). Produces the pinned, vendored
# read-only guest rootfs base.ext4 for the Firecracker Tier-3 backend (ADR 010 A1.Q1).
#
# The base bakes in ONLY the trusted guest binaries: a minimal Alpine userland (busybox sh + the
# system dirs the payload needs), the project-authored vsock->/proxy.sock shim (guest/rootfs/shim,
# part of the TCB), and /sbin/init (guest/rootfs/init/init). The per-run UNTRUSTED payload is never
# baked in — it arrives on a separate writable drive (/dev/vdb) at run time. This keeps the base
# digest stable: scan once, pin once, reset for free.
#
# Output: base.ext4 + base.ext4.sha256, written next to this script.
#
# Requires (build host only): docker, mkfs.ext4, go, the standard coreutils. None of these is a
# RUNTIME dependency — the produced base.ext4 is a plain file verified by a stdlib crypto/sha256
# loader before boot.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/../.." && pwd)"
ALPINE_TAG="${ALPINE_TAG:-alpine:3.20}"
IMG="$HERE/base.ext4"
SIZE_MB="${SIZE_MB:-64}"

work="$(mktemp -d)"
cleanup() { rm -rf "$work"; }
trap cleanup EXIT

echo "[build.sh] exporting ${ALPINE_TAG} rootfs via docker..."
docker pull "$ALPINE_TAG" >/dev/null
# Install the util-linux `setpriv` into the image before export: the guest init uses it to drop the
# payload to an unprivileged uid (65534) when a pids cap is requested, so RLIMIT_NPROC actually bites
# (the kernel does not enforce NPROC for a uid-0 process). The busybox `setpriv` applet lacks
# --reuid/--regid/--clear-groups, so the full util-linux binary is required (ADR 010 D4, task 016).
cid="$(docker create "$ALPINE_TAG" /bin/sh -c 'apk add --no-cache setpriv >/dev/null 2>&1 || true')"
docker start -a "$cid" >/dev/null 2>&1 || true
mkdir -p "$work/rootfs"
docker export "$cid" | tar -C "$work/rootfs" -xf -
docker rm "$cid" >/dev/null

echo "[build.sh] building the static vsock->/proxy.sock shim (CGO_ENABLED=0)..."
( cd "$HERE/shim" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	go build -trimpath -ldflags='-s -w' -o "$work/rootfs/sbin/guest-proxy-shim" . )

echo "[build.sh] installing /sbin/init..."
install -m 0755 "$HERE/init/init" "$work/rootfs/sbin/init"

# Ensure /usr/bin/sh resolves (Alpine ships /bin/sh -> busybox; the payload entry is /usr/bin/sh).
mkdir -p "$work/rootfs/usr/bin"
ln -sf /bin/busybox "$work/rootfs/usr/bin/sh"
# A few mount points init relies on.
mkdir -p "$work/rootfs/proc" "$work/rootfs/sys" "$work/rootfs/dev" "$work/rootfs/tmp" "$work/rootfs/run"
# The read-only root cannot host a live socket; init mounts a tmpfs at /run and the shim listens on
# /run/proxy.sock. /proxy.sock is a symlink to it so the payload's cross-tier contract (always talk
# to /proxy.sock) is preserved.
ln -sf /run/proxy.sock "$work/rootfs/proxy.sock"

echo "[build.sh] packing read-only ext4 image (${SIZE_MB}M)..."
rm -f "$IMG"
# mkfs.ext4 with -d populates the image from a directory without needing root/loop-mount.
mkfs.ext4 -q -F -L rootfs -d "$work/rootfs" "$IMG" "${SIZE_MB}M"

echo "[build.sh] computing sha256..."
( cd "$HERE" && sha256sum "base.ext4" | awk '{print $1}' > base.ext4.sha256 )

echo "[build.sh] done:"
ls -la "$IMG" "$HERE/base.ext4.sha256"
echo "[build.sh] base.ext4 sha256: $(cat "$HERE/base.ext4.sha256")"
echo "[build.sh] (REPO_ROOT=$REPO_ROOT)"
