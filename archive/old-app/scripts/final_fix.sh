#!/bin/bash
# Get temp pw, set permanent password via API, read back the hash format qB generated
cd /opt/AutoSeedRelay
TMP=$(docker logs qbittorrent 2>&1 | grep temporary | tail -1 | awk '{print $NF}')
echo "Temp: $TMP"

# Login
curl -s -c /tmp/qbc -X POST http://127.0.0.1:9021/api/v2/auth/login \
  --data-urlencode "username=admin" --data-urlencode "password=$TMP"

# Set password
curl -s -b /tmp/qbc -X POST http://127.0.0.1:9021/api/v2/app/setPreferences \
  --data-raw 'json={"web_ui_password":"CHANGE_ME"}'

# Read back qB-generated hash
sleep 2
echo "=== QB's actual hash ==="
cat ./data/qb-config/qBittorrent/qBittorrent.conf | grep -i password

# Test
echo "=== Verify ==="
curl -s -X POST http://127.0.0.1:9021/api/v2/auth/login \
  --data-urlencode "username=admin" --data-urlencode "password=CHANGE_ME" | head -c 40
echo ""

# Restart relay
docker restart autoseedrelay
sleep 6
docker logs autoseedrelay 2>&1 | grep "qb login" | tail -2
echo DONE
