# Stage 1: SteamCMD Builder
FROM debian:bookworm-slim AS builder

ENV DEBIAN_FRONTEND=noninteractive
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y ca-certificates curl tar lib32gcc-s1

RUN mkdir -p /steamcmd /pz-server && \
    curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf - -C /steamcmd && \
    /steamcmd/steamcmd.sh +@sSteamCmdForcePlatformType linux +login anonymous +force_install_dir /pz-server +app_update 380870 +quit

# Stage 2: Minimal Runtime
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates git openssh-client dos2unix && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

RUN useradd -m -d /home/steam -s /bin/bash steam
USER steam
WORKDIR /home/steam

# Copy only the installed game files, leaving steamcmd and 32-bit junk behind
COPY --from=builder --chown=steam:steam /pz-server /home/steam/pz-server

USER root
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY vsrania.ini /home/steam/vsrania.ini
COPY vsrania_SandboxVars.lua /home/steam/vsrania_SandboxVars.lua

RUN dos2unix /usr/local/bin/entrypoint.sh && \
    chmod +x /usr/local/bin/entrypoint.sh && \
    chown steam:steam /home/steam/vsrania.ini /home/steam/vsrania_SandboxVars.lua

USER steam
WORKDIR /home/steam/pz-server

EXPOSE 16261/udp 16261/tcp 16262/udp 16262/tcp 27015/tcp

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
