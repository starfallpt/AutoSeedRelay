import http.cookiejar, urllib.request, urllib.parse, json, subprocess, re, time

# Get temp password from qB logs
log = subprocess.run(['docker','logs','qbittorrent'], capture_output=True, text=True).stdout
pw_match = re.search(r'session:\s*(\S+)', log)
if not pw_match:
    pw_match = re.search(r'password is provided for this session:\s*(\S+)', log)
pw = pw_match.group(1) if pw_match else 'adminadmin'
print(f'Temp password: {pw}')

# Set permanent password via qB API (port 9021 -> localhost)
cj = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cj))

# Login
login_data = urllib.parse.urlencode({'username': 'admin', 'password': pw}).encode()
r = opener.open('http://127.0.0.1:9021/api/v2/auth/login', login_data)
print(f'Login: {r.status}')

# Set password
prefs = json.dumps({'web_ui_password': 'CHANGE_ME'}).encode()
req = urllib.request.Request('http://127.0.0.1:9021/api/v2/app/setPreferences', data=prefs, method='POST')
r2 = opener.open(req)
print(f'SetPassword: {r2.status}')

# Restart relay
subprocess.run(['docker', 'restart', 'autoseedrelay'], capture_output=True)
time.sleep(8)

# Check
log2 = subprocess.run(['docker', 'logs', 'autoseedrelay'], capture_output=True, text=True).stdout
for line in log2.split('\n'):
    if 'qb login' in line:
        print(line)
print('Done')
