import paramiko, time

c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
c.connect('1.2.3.4', port=10022, username='root', password='REDACTED', timeout=15)

# Write deployment script to VPS and execute
deploy_script = """#!/bin/bash
set -e
cd /opt/AutoSeedRelay

docker stop autoseedrelay qbittorrent 2>/dev/null || true
docker rm autoseedrelay qbittorrent 2>/dev/null || true
docker compose down --remove-orphans 2>/dev/null || true
rm -rf ./data/qb-config ./data/relay.db
mkdir -p ./data/downloads ./config

# Pre-create qBittorrent config with known password (bypasses temp-password mechanism)
python3 -c "
import hashlib, os, base64
password = 'CHANGE_ME'
salt = os.urandom(16)
key = hashlib.pbkdf2_hmac('sha512', password.encode(), salt, 100000, dklen=32)
encoded = base64.b64encode(salt + key).decode()
os.makedirs('./data/qb-config', exist_ok=True)
with open('./data/qb-config/qBittorrent.conf', 'w') as f:
    f.write('[Preferences]\\n')
    f.write('WebUI\\\\\\\\Password_PBKDF2=\\\"@ByteArray(' + encoded + ')\\\"\\n')
print('qBittorrent.conf pre-created with password:', password)
"

cat > config/relay.yaml << 'YAML'
sources: []
targets: []
qb:
  host: qbittorrent
  port: 8080
  username: admin
  password: CHANGE_ME
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

docker compose up -d
echo "Waiting 30s for qB startup..."
sleep 30

# Verify qB is accessible through the relay proxy (qB port is not published to host)
echo "Verifying qB through relay proxy..."
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST http://localhost:9020/qb/api/v2/auth/login --data 'username=admin&password=CHANGE_ME' 2>/dev/null)
echo "qB login via proxy: HTTP $HTTP_CODE"
if [ "$HTTP_CODE" != "200" ] && [ "$HTTP_CODE" != "204" ]; then
    echo "WARNING: qB login returned HTTP $HTTP_CODE"
    echo "Checking container logs for temp password..."
    docker logs qbittorrent 2>&1 | grep -i "password" | tail -5
fi
docker restart autoseedrelay
sleep 5

echo "=== STATUS ==="
docker ps --format "table {{.Names}}\t{{.Status}}"

echo "=== relay logs ==="
docker logs autoseedrelay 2>&1 | grep -E "qb login|engine started" | tail -3

echo "=== Web panel ==="
curl -s -o /dev/null -w "%{http_code}" http://localhost:9020/
echo ""

echo "=== qB proxy ==="
curl -s -o /dev/null -w "%{http_code}" http://localhost:9020/qb/api/v2/app/version
echo ""

echo "qB WebUI: http://1.2.3.4:9020/qb/"
echo "qB 密码: CHANGE_ME"
"""

c.exec_command("cat > /root/deploy.sh << 'SCRIPTEOF'\n" + deploy_script + "\nSCRIPTEOF", timeout=5)
c.exec_command("chmod +x /root/deploy.sh", timeout=5)
time.sleep(1)

_, o, e = c.exec_command("bash /root/deploy.sh", timeout=180)
print(o.read().decode())
err = e.read().decode()
if err.strip():
    print("STDERR:", err[:1000])
c.close()
