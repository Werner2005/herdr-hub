#!/usr/bin/env bash
set -euo pipefail

# This file lives in your persistent home and is run as dev on every start.
# Add custom APT repositories/keys here, before restoring their packages.
sudo /usr/local/bin/herdr-persist-packages --restore

# Add your own setup below. Keep configuration sources in your home, e.g.:
# sudo install -m 0644 "$HOME/.config/my-tool/system.conf" /etc/my-tool.conf
