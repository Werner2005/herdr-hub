#!/usr/bin/env bash
set -euo pipefail

# Build-time downloads only. Digests are reviewed together with version bumps;
# no mutable installer/latest manifest is executed at workspace startup.
case "${TARGETARCH:-$(uname -m)}" in
  amd64|x86_64)
    herdr_arch=x86_64
    herdr_sha=18a8dc65f1c2fa485884344356dea1cfd911c6f06cf46fa78e193f4087f4dba7
    bun_arch=x64-baseline
    bun_sha=1fc2edac843102909e3a1be1d8d9802cc6071cf074e67e88231f4ffe0f8b397b
    ;;
  arm64|aarch64)
    herdr_arch=aarch64
    herdr_sha=4de7aa3e25678812e92960de64f7c2aaa1bca1f0f80a3c5e559837e231e1f5c0
    bun_arch=aarch64
    bun_sha=efa9813da5ed72423bf847f916e8d2c47c0d776add972354026a75e10da9aa21
    ;;
  *) echo "Unsupported runtime architecture" >&2; exit 1 ;;
esac
temporary="$(mktemp -d)"
trap 'rm -rf -- "$temporary"' EXIT
download() {
  curl --fail --silent --show-error --location --retry 3 --connect-timeout 10 --max-time 180 \
    --proto '=https' --proto-redir '=https' "$1" -o "$2"
  printf '%s  %s\n' "$3" "$2" | sha256sum --check --status
}
destination=/usr/local/share/herdr/home-bin
mkdir -p "$destination"
download "https://github.com/herdrdev/herdr/releases/download/v0.9.3/herdr-linux-$herdr_arch" "$temporary/herdr" "$herdr_sha"
install -m 0755 "$temporary/herdr" "$destination/herdr"
download "https://github.com/oven-sh/bun/releases/download/bun-v1.4.3/bun-linux-$bun_arch.zip" "$temporary/bun.zip" "$bun_sha"
unzip -q "$temporary/bun.zip" -d "$temporary"
install -m 0755 "$temporary/bun-linux-$bun_arch/bun" "$destination/bun"
# v0.4.13 still declares the legacy plugin ID; pin the completed Ovis rename.
ref=5761411a0c784d104591b6df3ae007de68a494dc
download "https://raw.githubusercontent.com/devswha/ovis/$ref/install.sh" "$temporary/install.sh" 69d31a8b0104ee8e97858cf1c4bc2b819ae0f7873e1c1d3d8dfb88200b5066b8
install -m 0644 "$temporary/install.sh" /usr/local/share/herdr/ovis-install.sh
printf '%s\n' "$ref" > /usr/local/share/herdr/ovis-ref
