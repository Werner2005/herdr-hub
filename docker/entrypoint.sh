#!/usr/bin/env bash
set -euo pipefail

export OVIS_TOKEN="${OVIS_TOKEN:-${HERDR_WEB_TOKEN:-}}"
if [[ -z "$OVIS_TOKEN" ]]; then
  echo "OVIS_TOKEN (or legacy HERDR_WEB_TOKEN) must be set when Ovis is bound to ${HOST}:${PORT}." >&2
  exit 1
fi

# Retained homes keep their state in place; fresh homes use the Ovis directory.
export OVIS_STATE_DIR="${OVIS_STATE_DIR:-${HERDR_WEB_STATE_DIR:-}}"
if [[ -z "$OVIS_STATE_DIR" ]]; then
  OVIS_STATE_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/ovis"
  if [[ ! -d "$OVIS_STATE_DIR" && -d "${XDG_CONFIG_HOME:-$HOME/.config}/herdr-web-ui" ]]; then
    OVIS_STATE_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/herdr-web-ui"
  fi
fi

herdr-init-home
bash "$HOME/startup.sh"
sudo /usr/local/bin/herdr-update-packages --startup

ssh_host_key_dir=/home/dev/.ssh/hostkeys
mkdir -p /home/dev/.ssh /home/dev/workspace
sudo install -d -m 0700 -o root -g root "$ssh_host_key_dir" /run/sshd
if ! sudo test -f "$ssh_host_key_dir/ssh_host_ed25519_key"; then
  sudo ssh-keygen -q -t ed25519 -N '' -f "$ssh_host_key_dir/ssh_host_ed25519_key"
fi
# Kubernetes fsGroup can add group permissions when a PVC is remounted.
sudo chmod 0600 "$ssh_host_key_dir/ssh_host_ed25519_key"
sudo /usr/sbin/sshd -t -f /etc/ssh/sshd_config

herdr_pid=
sshd_pid=
updates_pid=
cleanup() {
  [[ -z "$updates_pid" ]] || sudo kill "$updates_pid" 2>/dev/null || true
  [[ -z "$herdr_pid" ]] || kill "$herdr_pid" 2>/dev/null || true
  [[ -z "$sshd_pid" ]] || sudo kill "$sshd_pid" 2>/dev/null || true
  [[ -z "$herdr_pid" ]] || wait "$herdr_pid" 2>/dev/null || true
  [[ -z "$sshd_pid" ]] || wait "$sshd_pid" 2>/dev/null || true
  [[ -z "$updates_pid" ]] || wait "$updates_pid" 2>/dev/null || true
}
trap cleanup EXIT INT TERM

sudo /usr/local/bin/herdr-update-packages &
updates_pid=$!

sudo /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config &
sshd_pid=$!

# Clear stale process bookkeeping before the installer tries to stop a legacy
# plugin during migration; its old PID could belong to a different process now.
rm -f "${XDG_STATE_HOME:-$HOME/.local/state}/herdr/plugins/devswha.ovis/server.pid" \
  "${XDG_STATE_HOME:-$HOME/.local/state}/herdr/plugins/devswha.herdr-web-ui/server.pid"
for lock in "$OVIS_STATE_DIR"/updates/*/supervisor.lock; do
  [[ ! -d "$lock" ]] || rm -rf -- "$lock"
done

herdr-install-node
if ! command -v herdr >/dev/null 2>&1 || ! command -v bun >/dev/null 2>&1 || \
    ! herdr plugin list | grep -F 'devswha.ovis ' >/dev/null || \
    herdr plugin list | grep -F 'devswha.herdr-web-ui ' >/dev/null; then
  # Existing user toolchains are preserved, but must be new enough to avoid the
  # official installer's mutable nested installers/automatic Bun upgrade.
  herdr_version="$(herdr --version | awk '{print $2}')"
  bun_version="$(bun --version)"
  [[ "$(printf '%s\n' 0.9.0 "$herdr_version" | sort -V | head -n1)" == 0.9.0 && \
     "$(printf '%s\n' 1.4.0 "$bun_version" | sort -V | head -n1)" == 1.4.0 ]] || {
    echo "Update the persistent-home Herdr (>=0.9.0) and Bun (>=1.4.0) tools before installing Ovis." >&2; exit 1;
  }
  OVIS_REF="$(cat /usr/local/share/herdr/ovis-ref)" SHELL=/bin/false \
    sh /usr/local/share/herdr/ovis-install.sh
fi

# Plugin credential configuration
plugin_config_dir="$(herdr plugin config-dir devswha.ovis)"
umask 077
# The Kubernetes Secret is authoritative, including when a retained home is
# reattached. Refuse symlink destinations; atomically replace with mode 0600.
[[ ! -L "$plugin_config_dir" && ! -L "$plugin_config_dir/env" && ! -L "$plugin_config_dir/.env" ]] || {
  echo "Refusing symlinked plugin credential configuration" >&2; exit 1;
}
# .env takes precedence over env in Ovis; synchronize both when it exists.
for env_name in env .env; do
  [[ "$env_name" == env || -f "$plugin_config_dir/$env_name" ]] || continue
  token_env="$(mktemp "$plugin_config_dir/.env-XXXXXX")"
  if [[ -f "$plugin_config_dir/$env_name" ]]; then
    # Preserve unrelated user settings, not stale credentials or bind settings.
    grep -Ev '^[[:space:]]*(export[[:space:]]+)?(HOST|PORT|OVIS_TOKEN|HERDR_WEB_TOKEN|OVIS_STATE_DIR|HERDR_WEB_STATE_DIR|BUN_OPTIONS)[[:space:]]*=' "$plugin_config_dir/$env_name" > "$token_env" || [[ $? == 1 ]]
  fi
  # Retained update builds may predate the rename and still read the old names.
  printf '\nHOST=%s\nPORT=%s\nOVIS_TOKEN=%s\nHERDR_WEB_TOKEN=%s\nOVIS_STATE_DIR=%s\nHERDR_WEB_STATE_DIR=%s\n' \
    "$HOST" "$PORT" "$OVIS_TOKEN" "$OVIS_TOKEN" "$OVIS_STATE_DIR" "$OVIS_STATE_DIR" >> "$token_env"
  printf 'BUN_OPTIONS=--preload=/usr/local/share/herdr/ovis-preload.ts\n' >> "$token_env"
  chmod 0600 "$token_env"
  mv -fT -- "$token_env" "$plugin_config_dir/$env_name"
done

cd /home/dev/workspace
herdr server &
herdr_pid=$!

for _ in $(seq 1 120); do
  if herdr status server >/dev/null 2>&1; then
    break
  fi
  sleep 1
done
herdr status server >/dev/null

herdr plugin action invoke devswha.ovis.start

for _ in $(seq 1 120); do
  if curl --fail --silent "http://127.0.0.1:${PORT}/api/health" >/dev/null; then
    break
  fi
  sleep 1
done
curl --fail --silent "http://127.0.0.1:${PORT}/api/health" >/dev/null

wait -n "$herdr_pid" "$sshd_pid" "$updates_pid"
