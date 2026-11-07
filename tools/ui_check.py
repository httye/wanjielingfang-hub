"""检查内置页面的路由与权限：/, /console, /admin, /assets。"""
import json
import sys
import urllib.error
import urllib.request
import uuid

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080").rstrip("/")
passed = failed = 0


def check(cond, msg):
    global passed, failed
    if cond:
        passed += 1
        print("  PASS " + msg)
    else:
        failed += 1
        print("  FAIL " + msg)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **k):
        return None


def request(method, path, body=None, headers=None, follow=False):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    op = urllib.request.build_opener() if follow else urllib.request.build_opener(NoRedirect)
    try:
        with op.open(req, timeout=20) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def jreq(method, path, body=None, headers=None):
    s, raw, hdrs = request(method, path, body, headers, follow=True)
    try:
        return s, (json.loads(raw.decode()) if raw else None), raw
    except Exception:
        return s, None, raw


print("== 根路径与未登录跳转 ==")
s, _, h = request("GET", "/")
check(s == 302 and h.get("Location") == "/console", "GET / → 302 /console")
s, _, h = request("GET", "/console")
check(s == 302 and "/auth/login" in (h.get("Location") or ""), "/console 未登录跳登录页")
s, _, h = request("GET", "/admin")
check(s == 302 and "redirect=/admin" in (h.get("Location") or "").replace("%2Fadmin", "/admin"),
      "/admin 未登录跳登录页并带回跳")

print("\n== 静态资源 ==")
s, body, h = request("GET", "/assets/app.css")
check(s == 200 and "text/css" in (h.get("Content-Type") or ""), "app.css 200 text/css")
s, body, h = request("GET", "/assets/app.js")
check(s == 200 and "javascript" in (h.get("Content-Type") or ""), "app.js 200 javascript")
s, _, _ = request("GET", "/assets/../web.go")
check(s != 200, "路径穿越不会返回 200")
s, body, _ = request("GET", "/assets/..%2Fweb.go")
check(s in (301, 302, 400, 404) and b"package web" not in body, "编码穿越不泄露源码")

print("\n== 管理员访问 ==")
s, adm, _ = jreq("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
ah = {"Authorization": "Bearer " + adm["token"]}
s, body, _ = request("GET", "/console", headers=ah)
html = body.decode("utf-8", "replace")
check(s == 200 and "data-view=\"tasks\"" in html, "/console 返回用户中心")
check("管理后台" in html, "管理员可见后台入口")
s, body, _ = request("GET", "/admin", headers=ah)
html = body.decode("utf-8", "replace")
check(s == 200 and "维护模式" in html, "/admin 返回管理后台")
check("价格规则" in html and "CDK" in html, "后台包含价格规则与 CDK 面板")

print("\n== 普通用户访问 /admin ==")
sfx = uuid.uuid4().hex[:8]
s, reg, _ = jreq("POST", "/api/register",
                 {"email": "ui_%s@example.com" % sfx, "username": "ui_" + sfx, "password": "Passw0rd!123"})
uh = {"Authorization": "Bearer " + reg["token"]}
s, body, _ = request("GET", "/console", headers=uh)
check(s == 200, "普通用户可进 /console")
s, body, _ = request("GET", "/admin", headers=uh)
check(s == 403, "普通用户访问 /admin 返回 403")

print("\n== 新增的后台接口 ==")
s, d, _ = jreq("GET", "/api/admin/tasks?limit=5", headers=ah)
check(s == 200 and isinstance(d.get("items"), list), "GET /api/admin/tasks")
s, d, _ = jreq("GET", "/api/admin/price-rules", headers=ah)
check(s == 200 and isinstance(d.get("items"), list), "GET /api/admin/price-rules")
s, _, _ = jreq("GET", "/api/admin/tasks", headers=uh)
check(s == 403, "普通用户调用后台接口 403")

print("\n========================================")
print("通过 %d 项，失败 %d 项" % (passed, failed))
sys.exit(1 if failed else 0)
