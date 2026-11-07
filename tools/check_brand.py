"""检查品牌名与页面标题是否更新到位。"""
import json
import re
import urllib.request

BASE = "http://127.0.0.1:8080"


def get(path, token=None):
    req = urllib.request.Request(BASE + path)
    if token:
        req.add_header("Authorization", "Bearer " + token)
    return urllib.request.urlopen(req).read().decode("utf-8")


def post(path, body):
    req = urllib.request.Request(
        BASE + path,
        data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    return json.load(urllib.request.urlopen(req))


tok = post("/api/login", {"identifier": "admin", "password": "admin123"})["token"]

cfg = json.loads(get("/api/config"))
print("brand.name   :", cfg["brand"]["name"])

lp = get("/auth/login")
print("登录页 title :", re.search(r"<title>([^<]*)</title>", lp).group(1))
print("登录页 h1    :", re.search(r"<h1>([^<]*)</h1>", lp).group(1))
print("登录页 署名  :", "万界零方" in lp)

for p in ("/console", "/admin"):
    html = get(p, tok)
    print("%-9s title: %-6s | 资源: %-16s | 署名: %s" % (
        p,
        re.search(r"<title>([^<]*)</title>", html).group(1),
        re.search(r"app\.css\?v=\d+", html).group(0),
        "万界零方" in html,
    ))
