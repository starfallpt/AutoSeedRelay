#!/bin/sh
TMP=$(docker logs qbittorrent 2>&1 | grep temporary | tail -1 | awk '{print $NF}')
echo "Temp: $TMP"

cat > /tmp/fixpw.py << 'PYEOF'
import http.cookiejar, urllib.request, urllib.parse, json, sys
pw = sys.argv[1]
cj = http.cookiejar.CookieJar()
op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
# Login
op.open('http://localhost:8080/api/v2/auth/login',
    urllib.parse.urlencode({'username': 'admin', 'password': pw}).encode())
# Set password using Request object for POST method
prefs = json.dumps({'web_ui_password': 'CHANGE_ME'}).encode()
req = urllib.request.Request('http://localhost:8080/api/v2/app/setPreferences', data=prefs)
req.add_header('Content-Type', 'application/json')
r2 = op.open(req)
print('SetPW:', r2.status)
PYEOF

docker cp /tmp/fixpw.py qbittorrent:/tmp/fixpw.py
docker exec qbittorrent python3 /tmp/fixpw.py "$TMP"

sed -i 's/PLACEHOLDER_PW/CHANGE_ME/' /opt/AutoSeedRelay/config/relay.yaml
docker restart autoseedrelay
sleep 8
docker logs autoseedrelay 2>&1 | grep "qb login" | tail -2
echo "Done"
