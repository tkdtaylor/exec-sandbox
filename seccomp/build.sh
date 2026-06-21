#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# build.sh — BUILD-TIME ONLY regenerator for the Tier-1 seccomp cBPF blob.
#
# Compiles gen.c against the host libseccomp, runs it over tier1-policy.json (the plain-text
# source of truth), and writes:
#   tier1.bpf         — the compiled cBPF program (committed; loaded at spawn via --seccomp <fd>)
#   tier1.bpf.sha256  — the pin (verified by the Go loader before the fd reaches bwrap)
#
# libseccomp is BUILD-TIME tooling only. The runtime Go path never links it: it open(2)s
# tier1.bpf and verifies it against tier1.bpf.sha256 with stdlib crypto/sha256. This preserves
# the project's stdlib-only invariant (ADR 016).
#
# Reproducibility (TC-019-08): libseccomp emits deterministic cBPF for the same ordered rule set
# on the same version, so re-running build.sh reproduces the committed sha256. If the host
# libseccomp version differs from PROVENANCE below, the bytes may differ — regenerate and pin in
# the same commit, recording the new version.
#
# PROVENANCE: blob generated with libseccomp 2.6.0, gcc 15.2.0, x86_64, default action
#             SCMP_ACT_ERRNO(EPERM). Update this line whenever the blob is regenerated.
set -euo pipefail

cd "$(dirname "$0")"

if ! pkg-config --exists libseccomp; then
  echo "build.sh: libseccomp dev package not found (pkg-config libseccomp). Install libseccomp-dev." >&2
  exit 1
fi

CFLAGS=$(pkg-config --cflags libseccomp)
LIBS=$(pkg-config --libs libseccomp)

tmpbin=$(mktemp)
trap 'rm -f "$tmpbin"' EXIT

# shellcheck disable=SC2086
cc $CFLAGS -O2 -Wall -o "$tmpbin" gen.c $LIBS

"$tmpbin" tier1-policy.json > tier1.bpf

# Pin: "<sha256>  tier1.bpf" — the same `sha256sum -c`-checkable format, but the Go loader reads
# only the leading hex field.
sha256sum tier1.bpf > tier1.bpf.sha256

echo "build.sh: wrote tier1.bpf ($(wc -c < tier1.bpf) bytes) and tier1.bpf.sha256"
sha256sum -c tier1.bpf.sha256
