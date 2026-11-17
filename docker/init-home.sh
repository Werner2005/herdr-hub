#!/usr/bin/env bash
set -euo pipefail

mkdir -p "$HOME/.local/bin" "$HOME/workspace" "$HOME/.ssh"

# Also treat dangling symlinks as existing user files. Never replace them.
seed_file() {
  local source=$1 destination=$2
  if [[ ! -e "$destination" && ! -L "$destination" ]]; then
    (set -o noclobber; cat "$source" > "$destination")
    chmod --reference="$source" "$destination"
  fi
}

for name in .profile .bashrc .bash_logout; do
  seed_file "/etc/skel/$name" "$HOME/$name"
done
seed_file /usr/local/share/herdr/startup.sh "$HOME/startup.sh"
for skill in /usr/local/share/herdr/skills/*; do
  [[ -d "$skill" ]] || continue
  destination="$HOME/.agents/skills/${skill##*/}"
  mkdir -p "$destination"
  seed_file "$skill/SKILL.md" "$destination/SKILL.md"
done
for name in codex opencode gemini claude uv uvx herdr bun; do
  seed_file "/usr/local/share/herdr/home-bin/$name" "$HOME/.local/bin/$name"
done
