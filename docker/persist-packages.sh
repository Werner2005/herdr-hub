#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C DEBIAN_FRONTEND=noninteractive

state_dir=/home/dev/.local/state/herdr/apt
manifest=$state_dir/packages.txt
baseline=/usr/local/share/herdr/apt-base-packages.txt
ready=/run/herdr-apt-ready

record_packages() {
  local temporary
  install -d -o dev -g dev /home/dev/.local/state /home/dev/.local/state/herdr "$state_dir"
  temporary=$(mktemp "$state_dir/.packages.XXXXXX")
  # Real files let pipeline failures preserve the previous manifest.
  if ! apt-mark showmanual | sort -u > "$temporary"; then
    rm -f "$temporary"
    return 1
  fi
  if ! comm -23 "$temporary" "$baseline" > "$temporary.filtered"; then
    rm -f "$temporary" "$temporary.filtered"
    return 1
  fi
  rm -f "$temporary"
  chmod 0644 "$temporary.filtered"
  chown dev:dev "$temporary.filtered"
  mv -f "$temporary.filtered" "$manifest"
}

case "${1:-}" in
  --record)
    # Do not overwrite the old list before startup has fully restored it.
    [[ -f "$ready" ]] || exit 0
    record_packages
    ;;
  --restore)
    rm -f "$ready"
    packages=()
    if [[ -f "$manifest" ]]; then
      while IFS= read -r package || [[ -n "$package" ]]; do
        [[ -n "$package" && "$package" != \#* ]] || continue
        if [[ ! "$package" =~ ^[a-z0-9][a-z0-9+.-]+(:[a-z0-9][a-z0-9-]*)?$ ]]; then
          printf 'Invalid saved APT package name: %s\n' "$package" >&2
          exit 1
        fi
        packages+=("$package")
      done < "$manifest"
    fi
    if (( ${#packages[@]} )); then
      apt-get update --error-on=any
      apt-get install -y --no-install-recommends -- "${packages[@]}"
    fi
    record_packages
    touch "$ready"
    ;;
  *)
    echo 'Usage: herdr-persist-packages --record|--restore' >&2
    exit 2
    ;;
esac
