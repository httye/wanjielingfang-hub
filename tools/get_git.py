"""从 expanded_assets 页面解析 PortableGit 真实文件名并下载。"""
import os
import re
import sys
import urllib.request

UA = {"User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"}
OUT_DIR = os.path.join(os.environ["USERPROFILE"], "tools")
OUT = os.path.join(OUT_DIR, "PortableGit.7z.exe")

with urllib.request.urlopen(
        urllib.request.Request("https://github.com/git-for-windows/git/releases/latest", headers=UA),
        timeout=30) as r:
    tag = r.geturl().rsplit("/", 1)[-1]

assets_url = "https://github.com/git-for-windows/git/releases/expanded_assets/" + tag
with urllib.request.urlopen(urllib.request.Request(assets_url, headers=UA), timeout=30) as r:
    html = r.read().decode("utf-8", "replace")

links = re.findall(r'href="(/git-for-windows/git/releases/download/[^"]+)"', html)
portable = [l for l in links if "PortableGit" in l and "64-bit" in l]
if not portable:
    print("assets not found. all links:")
    for l in links[:20]:
        print("  ", l)
    sys.exit(1)

url = "https://github.com" + portable[0]
print("tag :", tag)
print("url :", url)

os.makedirs(OUT_DIR, exist_ok=True)
if os.path.exists(OUT) and os.path.getsize(OUT) > 30_000_000:
    print("already downloaded:", OUT, round(os.path.getsize(OUT) / 1048576, 1), "MB")
    sys.exit(0)

print("downloading...")
total = 0
with urllib.request.urlopen(urllib.request.Request(url, headers=UA), timeout=600) as r, open(OUT, "wb") as f:
    while True:
        chunk = r.read(1 << 20)
        if not chunk:
            break
        f.write(chunk)
        total += len(chunk)
        if total % (20 << 20) < (1 << 20):
            print("  ...", round(total / 1048576), "MB")
print("done:", OUT, round(total / 1048576, 1), "MB")
