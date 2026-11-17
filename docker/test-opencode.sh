#!/usr/bin/env bash
set -euo pipefail

wrapper=$(realpath "${1:-$(dirname "$0")/opencode}")
temporary=$(mktemp -d)
trap 'rm -rf "$temporary"' EXIT

# A fresh workspace must approve only the package that prepares the launcher.
# Capture npx's arguments without downloading packages or starting a service.
cat > "$temporary/npx" <<'EOF'
#!/bin/bash
printf '%s\0' "$@" > "$NPX_CAPTURE"
exit "${NPX_STATUS:-0}"
EOF
chmod 755 "$temporary/npx"
export PATH="$temporary:$PATH" NPX_CAPTURE="$temporary/actual"

check_arguments() {
  printf '%s\0' --allow-scripts=@opencode/cli @opencode/cli@latest "$@" > "$temporary/expected"
  bash "$wrapper" "$@"
  cmp "$temporary/expected" "$NPX_CAPTURE"
}

check_arguments
check_arguments --version
check_arguments run 'a prompt with spaces' '' '--model=provider/model'

export NPX_STATUS=37
status=0
bash "$wrapper" --help || status=$?
[[ "$status" == 37 ]]
echo 'OpenCode wrapper regression checks passed.'
