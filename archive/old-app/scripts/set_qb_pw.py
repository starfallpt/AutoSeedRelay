import http.cookiejar, urllib.request, urllib.parse, json, subprocess, re

# Get temp password
log = subprocess.run(['docker','logs','qbittorrent'], capture_output=True, text=True).stdout
m = re.search(r'session:\s*(\S+)', log)
pw = m.group(1) if m else ''

# Set permanent password
cj = http.cookiejar.CookieJar()
o = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))
o.open('http://127.0.0.1:9021/api/v2/auth/login', urllib.parse.urlencode({'username':'admin','password':pw}).encode())
o.open('http://127.0.0.1:9021/api/v2/app/setPreferences', json.dumps({'web_ui_password':'CHANGE_ME'}).encode(), method='POST')
print('qB password set to CHANGE_ME')

# Restart relay
subprocess.run(['docker','restart','autoseedrelay'], capture_output=True)
print('Relay restarted')
