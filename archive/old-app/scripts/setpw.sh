#!/bin/bash
set -e
cd /opt/AutoSeedRelay

# Get temp password
TMP=$(docker logs qbittorrent 2>&1 | grep "temporary password" | tail -1 | awk '{print $NF}')
echo "Temp password: $TMP"

# Set permanent password via qB API from host (port 9021 mapped to localhost)
curl -s -c /tmp/qbc -X POST http://127.0.0.1:9021/api/v2/auth/login --data-urlencode "username=admin" --data-urlencode "password=$TMP"
curl -s -b /tmp/qbc -X POST http://127.0.0.1:9021/api/v2/app/setPreferences --data-raw 'json={"web_ui_password":"CHANGE_ME"}'

# Restart relay
docker restart autoseedrelay
sleep 8
docker logs autoseedrelay 2>&1 | grep "qb login" | tail -2
echo "Done"
