# Bun/JSC cannot reliably run under QEMU. Keep tests mandatory and native even
# when the workspace image is cross-built on the ARM CI runner.
FROM --platform=$BUILDPLATFORM oven/bun:1.4.3@sha256:ec06c3b6cea04192ae6770c434f668ca41d343ad19fa6472216c7b48be39c598 AS ovis-test
WORKDIR /ovis-test
COPY docker/ovis-preload.ts docker/ovis-preload.test.ts ./
RUN bun test ovis-preload.test.ts

FROM ubuntu:24.04@sha256:534baea6a22c03a63003dbc8dbe78fe34bc0d7e595d9a9dc9834884ff530eb55

ENV DEBIAN_FRONTEND=noninteractive \
    TZ=Europe/Berlin \
    SHELL=/bin/bash \
    HOME=/home/dev \
    PATH=/home/dev/.local/bin:/home/dev/.bun/bin:/usr/local/bin:/usr/bin:/bin \
    HOST=0.0.0.0 \
    PORT=7317 \
    OVIS_NO_OPEN=1

# CI changes this daily so security updates do not remain in a stale build cache.
ARG SECURITY_UPDATE_EPOCH=manual
RUN apt-get update && apt-get upgrade -y && apt-get install -y --no-install-recommends \
      ca-certificates curl git build-essential make autoconf automake libtool \
      pkg-config libssl-dev bash tini passwd sudo unzip openssh-server vim tzdata unattended-upgrades \
      coreutils findutils grep sed gawk diffutils patch less file ripgrep fd-find jq \
      python3 python3-venv shellcheck procps psmisc lsof tree rsync \
      iproute2 iputils-ping dnsutils netcat-openbsd wget openssh-client \
      tar gzip bzip2 xz-utils zip util-linux \
    && ln -snf /usr/share/zoneinfo/Europe/Berlin /etc/localtime \
    && printf 'Europe/Berlin\n' > /etc/timezone \
    && /usr/sbin/dpkg-reconfigure -f noninteractive tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && /usr/sbin/useradd --create-home --shell /bin/bash --uid 1001 --groups sudo dev \
    && printf 'dev:%s\n' "$(openssl rand -hex 32)" | /usr/sbin/chpasswd \
    && printf 'dev ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/dev \
    && chmod 0440 /etc/sudoers.d/dev \
    && mkdir -p /home/dev/workspace \
    && printf '#!/bin/sh\nexit 101\n' > /usr/sbin/policy-rc.d \
    && chmod 0755 /usr/sbin/policy-rc.d

COPY docker/apt-updates.conf /etc/apt/apt.conf.d/52herdr-container
COPY docker/apt-persistence.conf /etc/apt/apt.conf.d/53herdr-persistence
COPY docker/persist-packages.sh /usr/local/bin/herdr-persist-packages
COPY docker/startup.sh /usr/local/share/herdr/startup.sh
COPY docker/skills /usr/local/share/herdr/skills/
COPY docker/update-packages.sh /usr/local/bin/herdr-update-packages
COPY LICENCE.md THIRD_PARTY_NOTICES.md /usr/local/share/licenses/herdr-hub/
COPY licenses/ /usr/local/share/licenses/herdr-hub/third-party/
COPY docker/init-home.sh /usr/local/bin/herdr-init-home
COPY docker/download-runtime.sh /usr/local/share/herdr/download-runtime.sh
COPY --from=ovis-test /ovis-test/ovis-preload.ts /usr/local/share/herdr/ovis-preload.ts
ARG TARGETARCH
RUN bash /usr/local/share/herdr/download-runtime.sh
COPY docker/codex docker/opencode docker/gemini docker/claude /usr/local/share/herdr/home-bin/
COPY --from=ghcr.io/astral-sh/uv:0.13.0@sha256:cdc6093146eb3ff6a40107b38f008b789e050e77ad87865e381d9917da55a168 /uv /uvx /usr/local/share/herdr/home-bin/
RUN chmod 0755 /usr/local/bin/herdr-update-packages /usr/local/bin/herdr-init-home \
    /usr/local/bin/herdr-persist-packages /usr/local/share/herdr/startup.sh \
    /usr/local/share/herdr/home-bin/codex /usr/local/share/herdr/home-bin/opencode \
    /usr/local/share/herdr/home-bin/gemini /usr/local/share/herdr/home-bin/claude \
    && bash -n /usr/local/bin/herdr-update-packages /usr/local/bin/herdr-init-home \
      /usr/local/bin/herdr-persist-packages /usr/local/share/herdr/startup.sh \
      /usr/local/share/herdr/home-bin/codex /usr/local/share/herdr/home-bin/opencode \
      /usr/local/share/herdr/home-bin/gemini /usr/local/share/herdr/home-bin/claude \
    && ln -s /usr/bin/fdfind /usr/local/bin/fd \
    && apt-mark showmanual > /usr/local/share/herdr/apt-base-packages.txt \
    && LC_ALL=C sort -u -o /usr/local/share/herdr/apt-base-packages.txt /usr/local/share/herdr/apt-base-packages.txt

RUN --mount=type=bind,source=docker/test-opencode.sh,target=/tmp/test-opencode.sh \
    bash /tmp/test-opencode.sh /usr/local/share/herdr/home-bin/opencode

USER dev
WORKDIR /home/dev/workspace

COPY --chmod=0755 docker/entrypoint.sh /usr/local/bin/herdr-container-entrypoint
COPY --chmod=0755 docker/install-node.sh /usr/local/bin/herdr-install-node
COPY docker/sshd_config /etc/ssh/sshd_config
RUN bash -n /usr/local/bin/herdr-container-entrypoint /usr/local/bin/herdr-install-node \
    && herdr-init-home \
    && uv --version && uvx --version

EXPOSE 7317 2222
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/herdr-container-entrypoint"]
