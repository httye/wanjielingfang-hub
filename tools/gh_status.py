"""检查 GitHub 仓库状态与 PortableGit 下载可达性。"""
import json
import urllib.error
import urllib.request

UA = {"User-Agent": "wanjielingfang-hub-check"}
REPO = "httye/wanjielingfang-hub"


def get(url, headers=None):
    req = urllib.request.Request(url, headers=headers or UA)
    try:
        with urllib.request.urlopen(req, timeout=20) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()
    except Exception as e:
        return -1, str(e).encode()


s, body = get("https://api.github.com/repos/" + REPO)
if s == 200:
    d = json.loads(body)
    print("repo       :", d.get("full_name"))
    print("private    :", d.get("private"))
    print("size(KB)   :", d.get("size"))
    print("default    :", d.get("default_branch"))
    print("pushed_at  :", d.get("pushed_at"))
    print("html_url   :", d.get("html_url"))
else:
    print("repo check ->", s, body[:300].decode("utf-8", "replace"))

s, body = get("https://api.github.com/")
print("api root   ->", s)

s, body = get("https://github.com/git-for-windows/git/releases/latest", {"User-Agent": "Mozilla/5.0"})
print("git latest ->", s, "len=", len(body))
