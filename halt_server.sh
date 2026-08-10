#!/bin/bash
echo "Requesting server halt..."
touch halt_request
git add halt_request
git commit -m "Manual halt requested"
git push
echo "Halt request pushed to repository."
echo "The autosaver will now perform a final backup and send the shutdown command."
