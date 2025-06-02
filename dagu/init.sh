#!/bin/bash
set -e

INIT_DONE_FLAG="/tmp/init_completed"

# Check if initialization has already been done
if [ ! -f "$INIT_DONE_FLAG" ]; then
    echo "Running initialization scripts..."
    # Install Python dependencies or run other init scripts
    pip3 install uv --break-system-packages
    # Make sure to mount docker volume
    ls -la /root/.config/dagu/dags
    ls -la /root/.config/dagu/scripts
    pip3 install -r /root/.config/dagu/scripts/requirements.txt --break-system-packages

    # Create flag file to indicate initialization is complete
    touch "$INIT_DONE_FLAG"
    echo "Initialization complete."
fi
