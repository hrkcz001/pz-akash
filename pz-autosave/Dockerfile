FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates curl jq rsync openssh-client dos2unix python3 zip unzip git && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

COPY autosave.sh /usr/local/bin/autosave.sh
RUN dos2unix /usr/local/bin/autosave.sh && chmod +x /usr/local/bin/autosave.sh

WORKDIR /data

CMD ["/usr/local/bin/autosave.sh"]
