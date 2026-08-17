#!/bin/bash
# =============================================================================
# trigger.sh — webhook entry point. Spawned by webhook.py on every GitHub push
# to pz-saves: pulls the latest state and processes trigger files immediately
# (start/backup/halt). The main loop still runs for periodic backups, stop
# scheduling and cleanup.
# =============================================================================

set -uo pipefail
source /usr/local/bin/state.sh

echo "[trigger] $(date -u +%FT%TZ) webhook fired"
git_pull_state
process_triggers
echo "[trigger] done"
