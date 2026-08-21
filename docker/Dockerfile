# syntax=docker/dockerfile:1.7
#
# Both v2 images, from one file and one Go build.
#
#   docker build -f docker/Dockerfile --target controller -t pz-controller .
#   docker build -f docker/Dockerfile --target server     -t pz-server     .
#
# The build context is the repository root, and `--target controller` also needs
# the saves repository checked out at ./pz-saves — that is where the mod lists, the
# server .ini files and the dashboard's two extras (game.torrent, README.md) come
# from. The workflow in .github/workflows/images.yml does that with a second
# actions/checkout. A missing ./pz-saves fails the packages stage by design: an
# image with an empty /data/packages would deploy fine and serve nothing.
#
# What changed from v1's two Dockerfiles:
#
#   * One Go build replaces controller.sh, storage_server.py, webhook.py,
#     state.sh, schedule.sh, trigger.sh, deploy.sh, rcon.py, update_cloudflare.py,
#     entrypoint.sh and sdl.template. Nothing is COPYed into /usr/local/bin but a
#     single static binary, so there is no dos2unix step, no chmod +x sweep, and
#     no way for the two containers to disagree about a JSON field.
#   * SteamCMD is installed once, in a stage the packer and the game download
#     share. v1 had the same twenty lines in both files.
#   * The server image has no sshd, no sudo, no gosu, no python3, no zip, no jq
#     and no git. v1 needed all of them because backups left the container over
#     scp; in v2 the agent owns the process and uploads its own archives, so the
#     attack surface goes with the feature.
#   * Neither image declares EXPOSE. The ports live in config.yaml and reach Akash
#     through the rendered SDL; an EXPOSE line would be a second copy that cannot
#     be validated and would eventually be wrong.

# ---- pzctl: the whole of v2's logic, built once ----------------------------
FROM golang:1.25-bookworm AS pzctl

WORKDIR /src
# Dependencies first, so an edit to our own code does not re-download the module
# cache.
COPY pzctl/go.mod pzctl/go.sum ./
RUN go mod download

COPY pzctl/ ./

# VERSION reaches `pzctl version`, the state branches and the dashboard footer, so
# the tag on the image and the string in the logs are the same fact.
ARG VERSION=dev
# CGO off makes one static binary: it runs on debian:bookworm-slim, on scratch, or
# on a provider image we did not choose, and it takes the pure-Go DNS resolver
# with it. tzdata is embedded in the binary (identity.timezone must resolve even
# though neither image installs the tzdata package).
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/pzctl ./cmd/pzctl

# A binary that cannot print its own version has no business being copied into an
# image. This is the cheapest possible smoke test and it runs on every build.
RUN /out/pzctl version

# ---- steamcmd: shared by the packer and the game download ------------------
FROM debian:bookworm-slim AS steamcmd

ENV DEBIAN_FRONTEND=noninteractive
# SteamCMD is a 32-bit binary, hence the i386 architecture and the three lib32
# packages. This is the one place in v2 that needs them.
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates curl tar \
      lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /steamcmd && \
    curl -sSL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" \
      | tar zx -C /steamcmd && \
    /steamcmd/steamcmd.sh +login anonymous +quit || true
# The `|| true` is not laziness: steamcmd's first run updates itself and exits
# non-zero doing it. A real failure surfaces on the next invocation, which is not
# tolerated.

# ---- packages: mods, the three zips, the manifest --------------------------
FROM steamcmd AS packages

RUN apt-get update && \
    apt-get install -y --no-install-recommends python3 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Carried over from v1 unchanged, on purpose: it is build-time SteamCMD wrangling
# that already works, and rewriting it in Go would risk the mod lists for no
# runtime benefit. The v1 tree it came from is deleted at cutover, which is why
# there is a copy here.
COPY docker/build_packages.py /usr/local/bin/build_packages.py

# The saves. Fails the build when ./pz-saves is not in the context, which is the
# intended behaviour — see the header.
COPY pz-saves/ /build/pz-saves/

# Both default to false because the committed .ini already lists its mods and map;
# true lets the script rewrite Mods= and Map= from the downloaded set.
ARG AUTO_CONFIGURE_MODS=false
ARG AUTO_CONFIGURE_MAPS=false
RUN python3 /usr/local/bin/build_packages.py \
      --repo-path /build/pz-saves \
      --output-dir /packages \
      --steamcmd /steamcmd/steamcmd.sh \
      --auto-configure-mods "${AUTO_CONFIGURE_MODS}" \
      --auto-configure-maps "${AUTO_CONFIGURE_MAPS}"

# The dashboard's two optional extras. They are operator files at the root of
# pz-saves rather than build output, so the packer does not know about them:
# dashboard.torrent_file and dashboard.guide_file name them, and an absent file
# turns that part of the page off rather than 404ing a link the way v1 did.
RUN set -eu; \
    for f in game.torrent README.md README.ru.md README.en.md; do \
      if [ -f "/build/pz-saves/$f" ]; then cp "/build/pz-saves/$f" /packages/; fi; \
    done; \
    ls -la /packages

# ---- game: the Project Zomboid dedicated server ---------------------------
FROM steamcmd AS game

# 380870 is the dedicated server app. An ARG rather than a literal so a broken
# game build can be pinned by rebuilding, without editing this file.
ARG STEAM_APP_ID=380870
RUN /steamcmd/steamcmd.sh \
      +@sSteamCmdForcePlatformType linux \
      +login anonymous \
      +force_install_dir /pz-server \
      +app_update "${STEAM_APP_ID}" validate \
      +quit

# ---- controller ------------------------------------------------------------
FROM debian:bookworm-slim AS controller

ENV DEBIAN_FRONTEND=noninteractive
# ca-certificates and nothing else. The controller talks HTTPS to the Akash REST
# API, to Cloudflare and to GitHub, and go-git speaks SSH itself, so the git
# binary v1 shelled out to is not here — which also removes the `git reset --hard`
# that used to delete files out from under a running backup.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# A system user owning /data. v1 ran as root, and its only reason to was the sshd
# and the sudo that v2 does not have.
RUN useradd --system --uid 10001 --home-dir /data --create-home --shell /usr/sbin/nologin pzctl

COPY --from=pzctl /out/pzctl /usr/local/bin/pzctl
COPY --from=packages --chown=10001:10001 /packages /data/packages

# controller.storage.packages_dir, git.cache_dir and backups.dir, in that order.
# The last two are created at startup too; making them here means the image is
# already the shape the config describes.
RUN mkdir -p /data/repo /data/backups && chown -R 10001:10001 /data

USER pzctl
# The working directory is where bootstrap writes the config it fetches, so it has
# to be writable by this user and it has to be where config.Find looks.
WORKDIR /data

CMD ["pzctl", "controller"]

# ---- server ----------------------------------------------------------------
FROM debian:bookworm-slim AS server

ENV DEBIAN_FRONTEND=noninteractive
# The game ships its own JRE; libstdc++ and libgcc are in the base image. So this
# is ca-certificates for HTTPS to the controller, and that is the whole list — v1
# installed eleven packages here, seven of them for the ssh path that no longer
# exists.
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN useradd --create-home --home-dir /home/steam --shell /bin/bash steam

COPY --from=pzctl /out/pzctl /usr/local/bin/pzctl
COPY --from=game --chown=steam:steam /pz-server /home/steam/pz-server

# Set explicitly because Docker does not read /etc/passwd for USER: with HOME
# unset PZ would write its world to /Zomboid, which is not agent.paths.data_dir
# and is not writable. v1 got this by way of gosu.
ENV HOME=/home/steam

USER steam
WORKDIR /home/steam

# No ENTRYPOINT script. The agent is PID 1, which is the point: v1's entrypoint.sh
# ended with the server command, and when that returned the container restarted
# instead of dying — bug 2, the halt/restart loop, in one line of Dockerfile.
CMD ["pzctl", "agent"]
