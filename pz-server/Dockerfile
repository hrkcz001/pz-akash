# Stage 1: Download PZ Dedicated Server binaries using DepotDownloader
# DepotDownloader handles specific manifest downloads more reliably than
# SteamCMD's download_depot, which Valve has restricted for anonymous users.
FROM mcr.microsoft.com/dotnet/runtime:8.0-bookworm-slim AS builder

# Pin the exact PZ dedicated server version using Steam depot manifest IDs.
# To upgrade: look up new manifest IDs on https://steamdb.info/app/380870/depots/
ARG PZ_VERSION=42.20.2
# Depot 380871 = base server content, Depot 380873 = Linux-specific files
# Manifest IDs for 42.20.2 (August 5 2026) — find on SteamDB depot history pages
ARG DEPOT_380871_MANIFEST=2587362105419356756
ARG DEPOT_380873_MANIFEST=4894029153115054997

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl unzip && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Install DepotDownloader
RUN curl -sqL "https://github.com/SteamRE/DepotDownloader/releases/latest/download/DepotDownloader-linux-x64.zip" \
        -o /tmp/dd.zip && \
    mkdir -p /depotdownloader && \
    unzip /tmp/dd.zip -d /depotdownloader && \
    chmod +x /depotdownloader/DepotDownloader && \
    rm /tmp/dd.zip

# Download pinned depot manifests for PZ dedicated server.
# Uses Docker build secrets for Steam credentials (never baked into image layers).
RUN --mount=type=secret,id=steam_username \
    --mount=type=secret,id=steam_password \
    mkdir -p /pz-server && \
    STEAM_USER=$(cat /run/secrets/steam_username) && \
    STEAM_PASS=$(cat /run/secrets/steam_password) && \
    echo ">>> Downloading depot 380871 (manifest $DEPOT_380871_MANIFEST)..." && \
    /depotdownloader/DepotDownloader \
        -app 380870 -depot 380871 -manifest $DEPOT_380871_MANIFEST \
        -username "$STEAM_USER" -password "$STEAM_PASS" \
        -os linux -dir /pz-server && \
    echo ">>> Downloading depot 380873 (manifest $DEPOT_380873_MANIFEST)..." && \
    /depotdownloader/DepotDownloader \
        -app 380870 -depot 380873 -manifest $DEPOT_380873_MANIFEST \
        -username "$STEAM_USER" -password "$STEAM_PASS" \
        -os linux -dir /pz-server && \
    echo "$PZ_VERSION" > /pz-server/PINNED_VERSION

# Stage 2: PZ Dedicated Server Runtime
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates git openssh-client openssh-server dos2unix sudo curl jq gosu zip unzip python3 lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 && \
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
