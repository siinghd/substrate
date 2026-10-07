#!/usr/bin/env bash
# tools/rootfs/build-demo.sh — build the demo template rootfs.
#
#   process tier: produces a directory tree the shim seeds into sandbox workdirs
#   hv tier:      wraps the same tree into an ext4 image (requires root + mkfs.ext4)
#
# Usage: tools/rootfs/build-demo.sh [output-dir]
set -euo pipefail

OUT="${1:-tools/rootfs/demo-rootfs}"
ARCH="$(uname -m)"

echo "==> building demo rootfs (${ARCH}) into ${OUT}"
rm -rf "$OUT"
mkdir -p "$OUT"/{etc,root,tmp,workspace,run,usr/bin,usr/local/bin}

cat > "$OUT/etc/machine-id" </dev/null
printf 'nameserver 169.254.0.1\noptions edns0 trust-ad\n' > "$OUT/etc/resolv.conf"
printf 'root:x:0:0:root:/root:/bin/sh\n' > "$OUT/etc/passwd"

cat > "$OUT/workspace/README.md" <<'MD'
# Demo workspace

This is the substrate demo template. Try:

    subctl exec <sandbox> -- sh -c 'python3 --version; node --version'
MD

cat > "$OUT/root/.profile" <<'SH'
export PS1='sandbox> '
SH

if command -v python3 >/dev/null; then
  echo "    (host python3 present; process-tier sandboxes reuse host runtimes)"
fi

if [[ "${1:-}" == "--ext4" || "${SUBSTRATE_BUILD_EXT4:-0}" == "1" ]]; then
  IMG="${OUT}.ext4"
  SIZE_MB=512
  echo "==> packing ext4 image ${IMG} (${SIZE_MB}MiB)"
  truncate -s "${SIZE_MB}M" "$IMG"
  mkfs.ext4 -d "$OUT" -F "$IMG"
  echo "==> wrote $IMG"
fi

echo "==> done"
