# Stage 1: SteamCMD Game Server Builder (Downloads PZ Dedicated Server binaries)
FROM debian:bookworm-slim AS builder

# Pin the exact PZ dedicated server version using Steam depot manifest IDs.
# SteamCMD's app_update always grabs the latest, so we use download_depot
# with explicit manifest IDs to get a specific older version.
# To upgrade: look up new manifest IDs on https://steamdb.info/app/380870/depots/
ARG PZ_VERSION=42.20.2
# Depot 380871 = base server content, Depot 380873 = Linux-specific files
# Manifest IDs for 42.20.2 (August 5 2026) — find on SteamDB depot history pages
ARG DEPOT_380871_MANIFEST=2587362105419356756
ARG DEPOT_380873_MANIFEST=4894029153115054997

ENV DEBIAN_FRONTEND=noninteractive
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tar lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /steamcmd /pz-server && \
    curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf - -C /steamcmd && \
    /steamcmd/steamcmd.sh +login anonymous +quit || true

# Download pinned depot manifests for PZ dedicated server.
# SteamCMD anonymous downloads from CI/cloud IPs are prone to transient
# failures (exit code 14). Retry up to 5 times with cache clearing.
RUN set -e; \
    MAX_RETRIES=5; \
    for depot in "380871 $DEPOT_380871_MANIFEST" "380873 $DEPOT_380873_MANIFEST"; do \
        depot_id=$(echo $depot | cut -d' ' -f1); \
        manifest_id=$(echo $depot | cut -d' ' -f2); \
        attempt=1; \
        while [ $attempt -le $MAX_RETRIES ]; do \
            echo ">>> Downloading depot $depot_id (attempt $attempt/$MAX_RETRIES)..."; \
            if /steamcmd/steamcmd.sh +@sSteamCmdForcePlatformType linux +login anonymous \
                +download_depot 380870 $depot_id $manifest_id \
                +quit; then \
                echo ">>> Depot $depot_id downloaded successfully."; \
                break; \
            fi; \
            echo ">>> Depot $depot_id failed (attempt $attempt). Clearing cache and retrying in 15s..."; \
            rm -rf /steamcmd/steamapps/content/app_380870/depot_$depot_id; \
            sleep 15; \
            attempt=$((attempt + 1)); \
        done; \
        if [ $attempt -gt $MAX_RETRIES ]; then \
            echo ">>> ERROR: Failed to download depot $depot_id after $MAX_RETRIES attempts."; \
            exit 1; \
        fi; \
    done && \
    cp -r /steamcmd/steamapps/content/app_380870/depot_380871/* /pz-server/ && \
    cp -r /steamcmd/steamapps/content/app_380870/depot_380873/* /pz-server/ && \
    echo "$PZ_VERSION" > /pz-server/PINNED_VERSION

# Stage 2: PZ Dedicated Server Runtime
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates git openssh-client openssh-server dos2unix sudo curl jq gosu zip unzip python3 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /run/sshd

RUN useradd -m -d /home/steam -s /bin/bash steam
USER steam
WORKDIR /home/steam

# Copy installed game files from builder
COPY --from=builder --chown=steam:steam /pz-server /home/steam/pz-server

USER root
COPY entrypoint.sh /usr/local/bin/entrypoint.sh

RUN dos2unix /usr/local/bin/entrypoint.sh && \
    chmod +x /usr/local/bin/entrypoint.sh

WORKDIR /home/steam/pz-server

EXPOSE 16261/udp 16261/tcp 16262/udp 16262/tcp 27015/tcp 2222/tcp

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
