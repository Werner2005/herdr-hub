#!/usr/bin/env bash
set -euo pipefail

# Install into the persistent home as dev, never into a system-owned prefix.
node_version=26.11.0
npm_version=12.2.0
prefix="$HOME/.local"
export PATH="$prefix/bin:$PATH"
export NPM_CONFIG_PREFIX="$prefix"

# An existing persistent-home toolchain belongs to the user.
if command -v node >/dev/null 2>&1 && command -v npm >/dev/null 2>&1 && command -v npx >/dev/null 2>&1; then
  exit 0
fi

case "$(uname -m)" in
  x86_64)
    arch=x64
    checksum=2dcdf1a51a055e3ac1027ce5a01c2409ec7fa016b99e25e58a939568a9e0b6ad
    ;;
  aarch64|arm64)
    arch=arm64
    checksum=990bbeaf592bfae995def6c5b43b89bb5ddd222305c91251e3b9d9bfc7c120bc
    ;;
  *) echo "Unsupported Node.js architecture: $(uname -m)" >&2; exit 1 ;;
esac
release="$prefix/share/node/node-v${node_version}-linux-${arch}"

mkdir -p "$prefix/bin" "$prefix/share/node"
if [[ ! -x "$release/bin/node" ]]; then
  temporary="$(mktemp -d "$prefix/share/node/.install-XXXXXX")"
  trap 'rm -rf -- "$temporary"' EXIT
  archive="node-v${node_version}-linux-${arch}.tar.gz"
  curl --fail --silent --show-error --location --retry 3 \
    "https://nodejs.org/dist/v${node_version}/${archive}" -o "$temporary/$archive"
  printf '%s  %s\n' "$checksum" "$temporary/$archive" | sha256sum --check --status
  mkdir "$temporary/release"
  tar -xzf "$temporary/$archive" -C "$temporary/release" --strip-components=1
  mv "$temporary/release" "$release"
  rm -rf -- "$temporary"
  trap - EXIT
fi
if [[ ! -e "$prefix/bin/node" && ! -L "$prefix/bin/node" ]]; then
  ln -s "$release/bin/node" "$prefix/bin/node"
fi

# Persist the global prefix for interactive shells and SSH sessions as well.
if [[ ! -e "$HOME/.npmrc" && ! -L "$HOME/.npmrc" ]]; then
  (set -o noclobber; printf 'prefix=%s\n' "$prefix" > "$HOME/.npmrc")
fi
if [[ ! -e "$prefix/bin/npm" && ! -L "$prefix/bin/npm" && ! -e "$prefix/bin/npx" && ! -L "$prefix/bin/npx" ]]; then
  "$release/bin/npm" install --global --no-audit --no-fund "npm@$npm_version"
fi
printf 'Userspace tooling ready: Node.js %s, npm %s, npx %s\n' \
  "$(node --version)" "$(npm --version)" "$(npx --version)"
