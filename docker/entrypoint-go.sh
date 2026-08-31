#!/bin/sh
set -eu

data_dir="${ECS_DATA_DIR:-/var/lib/ecs-controller}"
mkdir -p "$data_dir"

# Keep legacy bind-mounted data readable without making the app process root.
if [ ! -f "$data_dir/data.sqlite" ] && [ -f /migration-data/data.sqlite ]; then
    cp -p /migration-data/data.sqlite "$data_dir/data.sqlite"
    if [ ! -f "$data_dir/.secret_encryption.key" ] && [ -f /migration-data/.secret_encryption.key ]; then
        cp -p /migration-data/.secret_encryption.key "$data_dir/.secret_encryption.key"
    fi
fi

chown -R ecs-controller:ecs-controller "$data_dir"
exec su-exec ecs-controller:ecs-controller /app/ecs-controller
