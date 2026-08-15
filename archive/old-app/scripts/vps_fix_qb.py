import paramiko, time

c = paramiko.SSHClient()
c.set_missing_host_key_policy(paramiko.AutoAddPolicy())
c.connect('1.2.3.4', port=10022, username='root', password='REDACTED', timeout=15)

# Fix 1: Update compose with correct env var name
compose = """services:
  relay:
    image: ghcr.io/starfallpt/autoseedrelay:latest
    container_name: autoseedrelay
    restart: unless-stopped
    ports:
      - "9020:9020"
    volumes:
      - ./config:/app/config:ro
      - ./data:/data
    depends_on:
      qbittorrent:
        condition: service_started

  qbittorrent:
    image: qbittorrentofficial/qbittorrent-nox:latest
    container_name: qbittorrent
    restart: unless-stopped
    expose:
      - "8080"
    environment:
      - QBT_WEBUI_PORT=8080
      - QBT_WEBUI_PASSWORD=CHANGE_ME
    volumes:
      - ./data/downloads:/downloads
      - ./data/qb-config:/config
"""

c.exec_command("cat > /opt/AutoSeedRelay/docker-compose.yml << 'YAMLEND'\n" + compose + "YAMLEND\n", timeout=5)

# Fix 2: Pre-create qB config with proper PBKDF2 hash
c.exec_command("""python3 -c "
import hashlib, os, base64
p = 'CHANGE_ME'
s = os.urandom(16)
k = hashlib.pbkdf2_hmac('sha512', p.encode(), s, 100000, dklen=32)
h = base64.b64encode(s + k).decode()
os.makedirs('/opt/AutoSeedRelay/data/qb-config', exist_ok=True)
with open('/opt/AutoSeedRelay/data/qb-config/qBittorrent.conf', 'w') as f:
    f.write('[Preferences]\\n')
    f.write('WebUI\\\\\\\\Password_PBKDF2=\\\"@ByteArray(' + h + ')\\\"\\n')
print('qB config pre-created')
" """, timeout=5)

# Fix 3: Update relay config with correct qB port
config = """sources: []
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
"""

c.exec_command("cat > /opt/AutoSeedRelay/config/relay.yaml << 'YAMLEND'\n" + config + "YAMLEND\n", timeout=5)

# Recreate everything fresh
_, o, e = c.exec_command("cd /opt/AutoSeedRelay && docker compose down && rm -rf ./data/qb-config && docker compose up -d && sleep 20 && docker ps --format 'table {{.Names}}\t{{.Status}}' && echo '=== qb test ===' && curl -s -o /dev/null -w '%{http_code}' http://localhost:9021/api/v2/app/version && echo '' && echo '=== relay ===' && docker logs autoseedrelay 2>&1 | grep 'qb login' | tail -2", timeout=180)
print(o.read().decode())
e2 = e.read().decode()
if e2.strip(): print('STDERR:', e2[:500])
c.close()
