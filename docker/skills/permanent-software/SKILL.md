---
name: permanent-software
description: Install software permanently in a Herdr workspace and persist its configuration. Use when installing APT packages, CLI tools, custom repositories, or editing /etc files that must survive container recreation.
---

# Permanent software in Herdr

## Persistence model

The workspace runs as `dev`, with passwordless `sudo`. Its persistent home is
`/home/dev`. Files there survive container recreation with the same home
volume/PVC, including `~/.config`, dotfiles, `~/.local`, `~/workspace`, and
`~/startup.sh`. Other separately mounted workspace volumes follow their own
mount configuration.

System directories such as `/usr`, `/opt`, `/var`, and `/etc` belong to the
container filesystem. Changes there do not survive container recreation unless
explicitly restored. A container restart alone is not proof of persistence;
verify using a fresh container attached to the same home volume when practical.

## Install Ubuntu packages

Use normal APT commands:

```sh
sudo apt-get update --error-on=any
sudo apt-get install -y --no-install-recommends ripgrep jq
```

Herdr's `DPkg::Post-Invoke` hook records manually installed packages beyond the
image defaults in `~/.local/state/herdr/apt/packages.txt`. Dependencies are
resolved by APT; they do not need separate entries. Removing packages through
APT updates the list:

```sh
sudo apt-get remove -y jq
```

At startup, the entrypoint initializes missing home files, runs `~/startup.sh`
as `dev`, then performs system updates before starting services. The seeded
startup script contains:

```sh
sudo /usr/local/bin/herdr-persist-packages --restore
```

Keep this call when editing the script. It restores saved packages from their
repositories using available versions, not pinned historical versions. A failed
restore stops startup and preserves the saved list for retry. Tracking is
enabled only after restoration succeeds, to avoid replacing the old list with
the fresh image's package list.

The hook does not run for standalone `apt-mark` changes or direct `dpkg -i`.
After those operations, refresh the list explicitly in a started workspace:

```sh
sudo /usr/local/bin/herdr-persist-packages --record
```

A saved package name is not a backup of a local `.deb`. For local packages,
keep the `.deb` in the home and install it before the restore call in
`~/startup.sh`, or make the package available through a repository.

## Custom repositories and other installers

For third-party APT packages, keep repository/key sources in the persistent
home and restore their `/etc/apt` configuration **before** the package restore
call in `~/startup.sh`. Follow the vendor's repository instructions; ensure the
repository supports the workspace's Ubuntu release and CPU architecture.

Prefer user-local installation when supported, for example binaries under
`~/.local/bin` or toolchains under `~/.local`. In this workspace, npm's global
prefix is `~/.local`, so use:

```sh
npm install -g <package>
```

without sudo. For software that must install outside the home, keep its installer
or source in the home and add an idempotent installation step to `~/startup.sh`.
Check whether the required version is already installed before downloading or
reinstalling it. Package tracking covers APT packages, not arbitrary installers.

## Persist /etc configuration

APT restores packages, **not your modified system configuration files**.
Editing `/etc/my-tool.conf` directly only changes the current container. Keep
the authoritative configuration in the persistent home and restore it after
the package restore call in `~/startup.sh`:

```sh
# Run once while setting up the tool; use the real file paths for your tool.
mkdir -p "$HOME/.config/my-tool"
sudo cp /etc/my-tool.conf "$HOME/.config/my-tool/system.conf"
sudo chown "$(id -u):$(id -g)" "$HOME/.config/my-tool/system.conf"
```

Edit the home copy, then add the following to the existing startup script:

```sh
sudo install -m 0644 "$HOME/.config/my-tool/system.conf" /etc/my-tool.conf
```

Use the ownership and permissions required by the actual application; sensitive
files may require mode `0600`. If a tool supports an explicit configuration path,
prefer pointing it directly at the home copy. For configuration needed to
resolve APT packages, restore it before the package restore call instead.

## Workflow and verification

1. Choose APT or a user-local installation appropriate to the software.
2. Install it and run a useful version or functionality check.
3. For APT, inspect `~/.local/state/herdr/apt/packages.txt` to confirm tracking.
4. Put durable configuration in the home; add any required restore steps to
   `~/startup.sh` without overwriting existing user commands.
5. Keep startup steps noninteractive and safe to repeat. Check shell syntax
   with `bash -n "$HOME/startup.sh"`. Execute specific new commands to apply
   configuration now; do not rerun unrelated startup commands blindly.
6. Tell the user where the software/configuration is stored and what will run
   on the next container start. Never claim `/etc` edits are automatically saved.

This skill is seeded only when missing at
`~/.agents/skills/permanent-software/SKILL.md`; user edits persist. Restart the
coding agent after initial installation so it discovers the skill.
