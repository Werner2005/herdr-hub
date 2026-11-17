# Third-party licensing

Herdr Hub's original code is licensed under the [MIT License](LICENCE.md).
This does not relicense its dependencies, downloaded tools, upstream installer,
base images or operating-system packages. Container images are not exclusively
MIT-licensed distributions.

## Herdr runtime

- Upstream: https://github.com/herdrdev/herdr
- Bundled release: **v0.9.3**
- License: **Apache License 2.0**
- Reviewed source: https://github.com/herdrdev/herdr/blob/v0.9.3/LICENSE
- Local license copy: [licenses/herdr-APACHE-2.0.txt](licenses/herdr-APACHE-2.0.txt)

The workspace downloads and runs the upstream Herdr binary as a separate
program; Herdr Hub does not change its license to MIT. Apache-2.0 permits this
distribution alongside MIT-licensed code, subject to its own conditions.
Redistribute the Apache license, retain applicable attribution/copyright
notices and mark any modified upstream files. Preserve applicable upstream
NOTICE attribution if a future release supplies it. The reviewed v0.9.3 source
tree has no top-level NOTICE file. Apache's patent and trademark provisions
continue to apply.

## Ovis

- Upstream: https://github.com/devswha/ovis
- Bundled installer/initial plugin revision:
  **5761411a0c784d104591b6df3ae007de68a494dc**
- License: **MIT**
- Reviewed source:
  https://github.com/devswha/ovis/blob/5761411a0c784d104591b6df3ae007de68a494dc/LICENSE
- Local license copy: [licenses/ovis-MIT.txt](licenses/ovis-MIT.txt)

Ovis's MIT terms are compatible with Herdr Hub's MIT license. Retain Ovis's
copyright notice, permission notice and disclaimer when distributing its
installer or substantial portions of its code. Its copyright remains
`Copyright (c) 2026 devswha`; Herdr Hub's license does not replace that notice.

## Scope and distribution

These checks cover the two pinned upstream projects above, not a complete
legal audit of every transitive dependency or container package. Other
components retain their own licenses and may impose additional redistribution
requirements. Review upstream licensing again when changing pins, downloading
new plugins or updating tools.

Workspace builds copy this notice, Herdr Hub's license and the two upstream
license texts into `/usr/local/share/licenses/herdr-hub/`. Ovis's installed
source retains its own license. The Hub image includes Herdr Hub's own license
at `/usr/share/licenses/herdr-hub/LICENCE.md`; it does not bundle the Herdr
runtime or Ovis installer.

Adding these files does not modify already published images or release tags.
Rebuild and publish a new version to distribute the added license files in
images. No additional rights to upstream trademarks are granted.
