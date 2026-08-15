#!/bin/bash
# One-command deploy: docker compose up, then auto-fix qB password
set -e
cd /opt/AutoSeedRelay
git pull

# Clean start
docker compose down --remove-orphans 2>/dev/null || true
docker rm -f autoseedrelay qbittorrent 2>/dev/null || true
rm -rf ./data/qb-config ./data/relay.db
mkdir -p ./data/downloads ./config

# Write relay config
cat > config/relay.yaml << 'YAML'
sources: []
targets: []
qb:
  host: qbittorrent
  port: 8080
  username: admin
  password: PLACEHOLDER_PW
strategy:
  role: publisher
  promotions: [free, 2x_free]
  keywords: [StarfallWeb]
  max_concurrent: 3
retire:
  min_seeders: 5
  min_ratio: 2.0
  min_days: 14
monitor:
  interval_seconds: 600
  disk_low_gb: 50
  disk_critical_gb: 20
  download_timeout: 3600
  retry_count: 3
  low_speed_kbps: 100
  low_speed_duration: 600
  low_speed_action: abort
poll_interval: 300
db_path: /data/relay.db
log_level: info
web:
  listen_addr: ":9020"
  password: admin
YAML

# Start
docker compose up -d
echo "Waiting for qB..."
sleep 35

# Fix qB password from INSIDE qB container (avoids host API issues)
TMP=$(docker logs qbittorrent 2>&1 | grep temporary | tail -1 | awk '{print $NF}')
echo "qB temp password: $TMP"

# Use Python inside qB container to set permanent password
docker exec qbittorrent python3 -c "
import http.cookiejar, urllib.request, urllib.parse, json
cj = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
op.open('http://localhost:8080/api/v2/auth/login',
    urllib.parse.urlencode({'username': 'admin', 'password': '$TMP'}).encode())
op.open('http://localhost:8080/api/v2/app/setPreferences',
    json.dumps({'web_ui_password': 'CHANGE_ME'}).encode(), method='POST')
print('Password set to CHANGE_ME')
"

# Update relay config with the permanent password
sed -i "s/PLACEHOLDER_PW/CHANGE_ME/" config/relay.yaml
docker restart autoseedrelay
sleep 6

echo "=== STATUS ==="
docker ps --format "table {{.Names}}\t{{.Status}}"
echo "=== relay qB ==="
docker logs autoseedrelay 2>&1 | grep "qb login" | tail -2
echo "=== Done ==="
echo "qB 密码: CHANGE_ME"
echo "Web: http://1.2.3.4:9020"
