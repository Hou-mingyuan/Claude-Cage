import json
import time
import urllib.error
import urllib.request
from pathlib import Path


def wait(config_path='/etc/claude-cage/browser-control.json', browser_url='http://127.0.0.1:18500/health', timeout=60):
    token = json.loads(Path(config_path).read_text())['token']
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    request = urllib.request.Request(browser_url, headers={'Authorization': 'Bearer ' + token})
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            with opener.open(request, timeout=2) as response:
                if json.loads(response.read(1024)).get('ready') is True:
                    return
        except (OSError, ValueError, urllib.error.URLError):
            pass
        time.sleep(1)
    raise SystemExit('Cage browser did not become ready. Inspect the guest service logs and retry.')


if __name__ == '__main__':
    wait()
