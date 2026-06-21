// SPDX-License-Identifier: Apache-2.0
//
// gen.c — build-time-only cBPF generator for the exec-sandbox Tier-1 seccomp profile.
//
// Reads the deny/allow syscall lists from seccomp/tier1-policy.json (the plain-text source of
// truth), builds the filter with libseccomp (default action SCMP_ACT_ERRNO(EPERM), every allow[]
// name SCMP_ACT_ALLOW, every deny[] name kept at the default deny), and writes the compiled cBPF
// program to stdout via seccomp_export_bpf(). Invoked only by seccomp/build.sh; the runtime Go
// loader never links this — it just open(2)s the resulting tier1.bpf (stdlib os + crypto/sha256).
//
// This is a deliberately small JSON reader, not a general parser: it scans the file for the
// "allow" and "deny" arrays and pulls the quoted token names out of each. The policy file is
// committed and trusted (it is our own source of truth), so a forgiving scan is sufficient and
// avoids pulling in a JSON library at build time.
//
// Determinism: libseccomp emits the same cBPF bytes for the same ordered rule set on the same
// version, so build.sh can byte-pin the output by sha256 (TC-019-08 reproducibility).

#include <seccomp.h>
#include <errno.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

// readfile slurps the whole policy file into a NUL-terminated buffer.
static char *readfile(const char *path, size_t *len) {
    FILE *f = fopen(path, "rb");
    if (!f) { perror("fopen policy"); exit(2); }
    fseek(f, 0, SEEK_END);
    long n = ftell(f);
    fseek(f, 0, SEEK_SET);
    char *buf = malloc((size_t)n + 1);
    if (!buf) { fprintf(stderr, "oom\n"); exit(2); }
    if (fread(buf, 1, (size_t)n, f) != (size_t)n) { perror("fread policy"); exit(2); }
    buf[n] = '\0';
    fclose(f);
    if (len) *len = (size_t)n;
    return buf;
}

// apply_array finds the JSON array named `key` (e.g. "allow") and applies `action` to every
// syscall name in it. The scan runs from the `"key"` token to the closing ']'. Returns the
// number of names applied. A name libseccomp does not know on this build is skipped with a
// warning rather than aborting (kernels differ in their syscall tables); a name present means it
// is governed by `action`, which is the contract the deny[]/allow[] lists encode.
static int apply_array(scmp_filter_ctx ctx, const char *json, const char *key, uint32_t action) {
    char needle[64];
    snprintf(needle, sizeof needle, "\"%s\"", key);
    const char *p = strstr(json, needle);
    if (!p) { fprintf(stderr, "policy: array %s not found\n", key); exit(2); }
    p = strchr(p, '[');
    if (!p) { fprintf(stderr, "policy: array %s has no '['\n", key); exit(2); }
    p++;
    const char *end = strchr(p, ']');
    if (!end) { fprintf(stderr, "policy: array %s has no ']'\n", key); exit(2); }

    int applied = 0;
    while (p < end) {
        const char *q = memchr(p, '"', (size_t)(end - p));
        if (!q || q >= end) break;
        q++;
        const char *r = memchr(q, '"', (size_t)(end - q));
        if (!r) break;
        size_t namelen = (size_t)(r - q);
        char name[64];
        if (namelen == 0 || namelen >= sizeof name) { p = r + 1; continue; }
        memcpy(name, q, namelen);
        name[namelen] = '\0';

        int sysnum = seccomp_syscall_resolve_name(name);
        if (sysnum == __NR_SCMP_ERROR) {
            fprintf(stderr, "warn: syscall '%s' unknown on this build; skipped\n", name);
        } else {
            if (seccomp_rule_add(ctx, action, sysnum, 0) < 0) {
                fprintf(stderr, "seccomp_rule_add failed for %s\n", name);
                exit(2);
            }
            applied++;
        }
        p = r + 1;
    }
    return applied;
}

int main(int argc, char **argv) {
    if (argc != 2) {
        fprintf(stderr, "usage: %s <tier1-policy.json>  (cBPF written to stdout)\n", argv[0]);
        return 2;
    }
    char *json = readfile(argv[1], NULL);

    // Default-deny: every syscall not explicitly allowed returns EPERM. This is the Docker-default
    // action and the ADR-016 choice (a visible errno the payload can observe, not a silent kill).
    scmp_filter_ctx ctx = seccomp_init(SCMP_ACT_ERRNO(EPERM));
    if (!ctx) { fprintf(stderr, "seccomp_init failed\n"); return 2; }

    // Pin the architecture so the committed blob is reproducible regardless of the build host's
    // native arch detection. Tier-1 targets x86_64.
    seccomp_arch_remove(ctx, SCMP_ARCH_NATIVE);
    if (seccomp_arch_add(ctx, SCMP_ARCH_X86_64) < 0) {
        fprintf(stderr, "seccomp_arch_add x86_64 failed\n"); return 2;
    }

    // allow[] → SCMP_ACT_ALLOW. deny[] names are documented/tested but need no rule: the default
    // action already denies them. Adding an explicit ERRNO rule for them would be redundant and is
    // intentionally omitted so the deny set's enforcement comes from the default-deny posture
    // (a name absent from allow[] is denied, which is the property the tests assert).
    int allowed = apply_array(ctx, json, "allow", SCMP_ACT_ALLOW);
    fprintf(stderr, "tier1: %d syscalls allowed; default action EPERM\n", allowed);

    // Export the cBPF program to stdout (fd 1).
    if (seccomp_export_bpf(ctx, 1) < 0) {
        fprintf(stderr, "seccomp_export_bpf failed\n");
        return 2;
    }
    seccomp_release(ctx);
    free(json);
    return 0;
}
