FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# Install only minimal required dependencies.
# Note: Project Zomboid ships with its own JRE, so we don't need openjdk.
RUN dpkg --add-architecture i386 && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
    ca-certificates curl git openssh-client tar \
    lib32gcc-s1 lib32stdc++6 libc6:i386 libstdc++6:i386 libsdl2-2.0-0 dos2unix \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

RUN useradd -m -d /home/steam -s /bin/bash steam
USER steam
WORKDIR /home/steam

RUN mkdir -p /home/steam/steamcmd && \
    cd /home/steam/steamcmd && \
    curl -sqL "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz" | tar zxvf - && \
    ./steamcmd.sh +login anonymous +quit || true

RUN mkdir -p /home/steam/pz-server && \
    cd /home/steam/steamcmd && \
    ./steamcmd.sh +@sSteamCmdForcePlatformType linux +login anonymous +force_install_dir /home/steam/pz-server +app_update 380870 +quit

USER root
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
COPY vsrania.ini /home/steam/vsrania.ini
COPY vsrania_SandboxVars.lua /home/steam/vsrania_SandboxVars.lua

# Fix windows line endings if any, and set permissions
RUN dos2unix /usr/local/bin/entrypoint.sh && \
    chmod +x /usr/local/bin/entrypoint.sh && \
    chown steam:steam /home/steam/vsrania.ini /home/steam/vsrania_SandboxVars.lua

USER steam
WORKDIR /home/steam/pz-server

EXPOSE 16261/udp 16262/udp 27015/tcp

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
