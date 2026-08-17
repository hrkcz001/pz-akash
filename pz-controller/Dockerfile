# Stage 1: SteamCMD & Mod Package Builder
FROM debian:bookworm-slim AS builder

ENV DEBIAN_FRONTEND=noninteractive
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tar lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 jq python3 git zip unzip && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /steamcmd /build/pz-saves /packages && \
    curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf - -C /steamcmd && \
    /steamcmd/steamcmd.sh +login anonymous +quit || true

ARG REPO_URL="https://github.com/hrkcz001/pz-saves.git"
ARG REPO_BRANCH="main"

COPY build_packages.py /usr/local/bin/build_packages.py
RUN chmod +x /usr/local/bin/build_packages.py

# Clone pz-saves (if public/available) or create template structure, then build packages
RUN git clone --depth 1 --branch "${REPO_BRANCH}" "${REPO_URL}" /build/pz-saves 2>/dev/null || mkdir -p /build/pz-saves/common /build/pz-saves/client /build/pz-saves/server
RUN python3 /usr/local/bin/build_packages.py --repo-path /build/pz-saves --output-dir /packages --steamcmd /steamcmd/steamcmd.sh

# Stage 2: Controller Runtime
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl jq rsync openssh-client dos2unix python3 zip unzip git && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY controller.sh /usr/local/bin/controller.sh
COPY storage_server.py /usr/local/bin/storage_server.py
COPY build_packages.py /usr/local/bin/build_packages.py
COPY rcon.py /usr/local/bin/rcon.py
COPY deploy.sh /usr/local/bin/deploy.sh
COPY state.sh /usr/local/bin/state.sh
COPY schedule.sh /usr/local/bin/schedule.sh
COPY trigger.sh /usr/local/bin/trigger.sh
COPY webhook.py /usr/local/bin/webhook.py
COPY webhook_url.sh /usr/local/bin/webhook_url.sh
COPY sdl.template /usr/local/bin/sdl.template

# Alias autosave.sh to controller.sh for backward compatibility
RUN ln -s /usr/local/bin/controller.sh /usr/local/bin/autosave.sh

RUN dos2unix /usr/local/bin/*.sh /usr/local/bin/*.py /usr/local/bin/sdl.template && \
    chmod +x /usr/local/bin/*.sh /usr/local/bin/*.py

# Copy prebuilt packages from builder stage
COPY --from=builder /packages /data/packages

# Bundle any local backups dropped into the local_backups folder
COPY local_backups/ /data/backups/

WORKDIR /data

CMD ["/usr/local/bin/controller.sh"]
