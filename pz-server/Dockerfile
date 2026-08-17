# Stage 1: SteamCMD Game Server Builder (Downloads PZ Dedicated Server binaries)
FROM debian:bookworm-slim AS builder

ENV DEBIAN_FRONTEND=noninteractive
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl tar lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /steamcmd /pz-server && \
    curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf - -C /steamcmd && \
    /steamcmd/steamcmd.sh +login anonymous +quit || true

# Download dedicated server (App ID 380870)
RUN /steamcmd/steamcmd.sh +@sSteamCmdForcePlatformType linux +login anonymous +force_install_dir /pz-server +app_update 380870 validate +quit

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
