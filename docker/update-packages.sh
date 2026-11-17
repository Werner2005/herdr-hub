#!/usr/bin/env bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive

if [[ "${1:-}" == --startup ]]; then
  apt-get update --error-on=any
  apt-get dist-upgrade -y
  exit 0
fi

# No systemd/cron in the workspace: run security updates every 24 hours.
sleep_pid=
trap '[[ -z "$sleep_pid" ]] || kill "$sleep_pid" 2>/dev/null || true; exit 0' TERM INT
while true; do
  sleep 86400 &
  sleep_pid=$!
  wait "$sleep_pid"
  sleep_pid=
  if ! (apt-get update --error-on=any && unattended-upgrade --verbose); then
    echo 'Security updates failed; retrying at the next daily interval.' >&2
  fi
done
