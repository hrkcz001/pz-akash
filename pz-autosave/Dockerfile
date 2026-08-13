FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl jq rsync openssh-client dos2unix python3 python3-pip zip unzip git && \
    apt-get clean && rm -rf /var/lib/apt/lists/*
RUN pip3 install uploadserver --break-system-packages || pip3 install uploadserver

COPY autosave.sh /usr/local/bin/autosave.sh
COPY rcon.py /usr/local/bin/rcon.py
COPY deploy.sh /usr/local/bin/deploy.sh
COPY state.sh /usr/local/bin/state.sh
COPY schedule.sh /usr/local/bin/schedule.sh
COPY trigger.sh /usr/local/bin/trigger.sh
COPY webhook.py /usr/local/bin/webhook.py
COPY webhook_url.sh /usr/local/bin/webhook_url.sh
COPY sdl.template /usr/local/bin/sdl.template
RUN dos2unix /usr/local/bin/autosave.sh /usr/local/bin/rcon.py /usr/local/bin/deploy.sh /usr/local/bin/state.sh /usr/local/bin/schedule.sh /usr/local/bin/trigger.sh /usr/local/bin/webhook.py /usr/local/bin/webhook_url.sh /usr/local/bin/sdl.template && chmod +x /usr/local/bin/autosave.sh /usr/local/bin/deploy.sh /usr/local/bin/state.sh /usr/local/bin/schedule.sh /usr/local/bin/trigger.sh /usr/local/bin/webhook_url.sh

# Bundle any local backups dropped into the local_backups folder
COPY local_backups/ /data/backups/

WORKDIR /data

CMD ["/usr/local/bin/autosave.sh"]
