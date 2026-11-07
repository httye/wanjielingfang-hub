"""MC AI 主站端到端冒烟（只用标准库，Windows/ Linux 通用）。

用法:
    python smoke.py [base_url]
默认 base_url = http://127.0.0.1:8080

覆盖：健康检查 → 注册(余额0) → 登录 → admin → CDK 生成/兑换 → 建站点 →
站点配置(含 ETag 304) → AI 对话 SSE → 生成任务(扣10) → 幂等(只扣一次) →
worker 注册/认领/上报 → 任务 done → 签名下载 → 首单赠送(+20) →
市场发布/购买/下载 → 订阅 → 后台统计 → 改配置触发适配任务。
"""

import io
import json
import os
import sys
import urllib.error
import urllib.request
import uuid

BASE = (sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8080").rstrip("/")

passed = 0
failed = 0


def check(cond, msg):
    global passed, failed
    if cond:
        passed += 1
        print("  PASS " + msg)
    else:
        failed += 1
        print("  FAIL " + msg)


def section(title):
    print("\n== %s ==" % title)


def call(method, path, body=None, headers=None, timeout=40):
    url = BASE + path
    data = None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    if body is not None:
        data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read(), dict(resp.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


def jcall(method, path, body=None, headers=None):
    status, raw, hdrs = call(method, path, body, headers)
    try:
        obj = json.loads(raw.decode("utf-8")) if raw else None
    except Exception:
        obj = None
    return status, obj, raw, hdrs


def multipart(path, fields, files, headers):
    boundary = "----mcai" + uuid.uuid4().hex
    buf = io.BytesIO()
    for k, v in fields.items():
        buf.write(("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n"
                   % (boundary, k, v)).encode("utf-8"))
    for k, (fname, data) in files.items():
        buf.write(("--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\n"
                   "Content-Type: application/octet-stream\r\n\r\n" % (boundary, k, fname)).encode("utf-8"))
        buf.write(data)
        buf.write(b"\r\n")
    buf.write(("--%s--\r\n" % boundary).encode("utf-8"))

    h = dict(headers or {})
    h["Content-Type"] = "multipart/form-data; boundary=" + boundary
    req = urllib.request.Request(BASE + path, data=buf.getvalue(), headers=h, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


# ---------------------------------------------------------------- 0
section("0 健康检查")
status, obj, _, _ = jcall("GET", "/healthz")
if status != 200 or not obj or not obj.get("ok"):
    print("  主站不可用: %s —— 先运行 hub.exe 或 go run ./cmd/hub" % BASE)
    sys.exit(1)
check(True, "GET /healthz 返回 ok")

# ---------------------------------------------------------------- 1
section("1 注册（余额应为 0）")
suffix = uuid.uuid4().hex[:8]
email = "u_%s@example.com" % suffix
password = "Passw0rd!123"
status, reg, _, _ = jcall("POST", "/api/register",
                          {"email": email, "username": "u_" + suffix, "password": password})
check(status == 201 and reg and len(reg.get("token", "")) > 20, "注册返回 token")
check(reg and reg.get("user", {}).get("balance") == 0, "新账号余额为 0（赠送延后到首单成功）")
utok = (reg or {}).get("token", "")
uh = {"Authorization": "Bearer " + utok}

status, me, _, _ = jcall("GET", "/api/me", headers=uh)
check(status == 200 and me.get("email") == email, "GET /api/me 返回本人")

# ---------------------------------------------------------------- 2
section("2 管理员登录 + CDK")
status, adm, _, _ = jcall("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
check(status == 200 and adm and adm.get("user", {}).get("role") == "admin", "admin 登录成功")
ah = {"Authorization": "Bearer " + (adm or {}).get("token", "")}

status, cdk, _, _ = jcall("POST", "/api/admin/cdks", {"count": 2, "amount": 100, "batch": "smoke"}, ah)
check(status == 201 and len(cdk.get("codes", [])) == 2, "生成 2 张 CDK")
codes = (cdk or {}).get("codes", ["", ""])

status, rd, _, _ = jcall("POST", "/api/redeem", {"code": codes[0]}, uh)
check(status == 200 and rd.get("balance") == 100, "兑换 CDK 后余额 100")

# ---------------------------------------------------------------- 3
section("3 公开配置 + 站点 + 站点配置下发")
status, pub, _, _ = jcall("GET", "/api/config")
check(status == 200 and pub.get("billing", {}).get("task_cost") is not None, "GET /api/config 含 billing.task_cost")
check(pub.get("config_version", 0) > 0, "config_version 存在")

status, st, _, _ = jcall("POST", "/api/sites",
                         {"name": "smoke-site", "base_url": "https://smoke.example.com"}, uh)
check(status == 201 and str(st.get("site_key", "")).startswith("sk_"), "创建站点返回 sk_ key")
sitkey = (st or {}).get("site_key", "")
sh = {"X-Site-Key": sitkey}

status, sc, _, hdr = jcall("GET", "/api/v1/site/config", headers=sh)
check(status == 200 and sc.get("config_version", 0) > 0, "站点配置带 config_version")
check(sc.get("catalog", {}).get("mc_versions") is not None, "站点配置含版本矩阵")
check("ai_api_key" not in sc, "站点配置不含 AI 密钥")

etag = hdr.get("ETag") or hdr.get("Etag")
status, _, raw, _ = jcall("GET", "/api/v1/site/config",
                          headers={"X-Site-Key": sitkey, "If-None-Match": etag or ""})
check(status == 304, "ETag 协商返回 304")

# ---------------------------------------------------------------- 4
section("4 AI 代理（mock 流式）")
status, sse, raw, _ = jcall("POST", "/api/v1/site/ai/chat",
                            {"mode": "restate", "messages": [{"role": "user", "content": "做一个抽奖插件"}]},
                            {"X-Site-Key": sitkey, "Authorization": "Bearer " + utok})
text = raw.decode("utf-8", "replace")
check(status == 200 and "data:" in text, "SSE 返回 data: 事件")
check('"done": true' in text or '"done":true' in text, "SSE 以 done 结束")

# ---------------------------------------------------------------- 5
section("5 生成任务（扣 10 积分）")
status, gen, _, _ = jcall("POST", "/api/v1/site/ai/generate",
                          {"platform": "paper", "mc_version": "1.20.4",
                           "requirement": "抽奖插件：/lottery 打开奖池，冷却 60 秒"},
                          {"X-Site-Key": sitkey, "Authorization": "Bearer " + utok})
check(status == 201 and gen.get("task_id", 0) > 0, "生成返回 task_id")
check(gen.get("cost") == 10, "计价 10 积分")
task_id = (gen or {}).get("task_id", 0)

status, after_gen, _, _ = jcall("GET", "/api/me", headers=uh)
check(after_gen.get("balance") == 90, "生成后余额 90")

idem = str(uuid.uuid4())
s1, g1, _, _ = jcall("POST", "/api/v1/site/ai/generate",
                     {"platform": "paper", "mc_version": "1.20.4", "requirement": "幂等测试"},
                     {"X-Site-Key": sitkey, "Authorization": "Bearer " + utok, "Idempotency-Key": idem})
s2, g2, _, _ = jcall("POST", "/api/v1/site/ai/generate",
                     {"platform": "paper", "mc_version": "1.20.4", "requirement": "幂等测试"},
                     {"X-Site-Key": sitkey, "Authorization": "Bearer " + utok, "Idempotency-Key": idem})
check(g1.get("task_id") == g2.get("task_id"), "相同 Idempotency-Key 返回同一任务")
status, after_idem, _, _ = jcall("GET", "/api/me", headers=uh)
check(after_idem.get("balance") == 80, "幂等提交只扣一次（余额 80）")

# ---------------------------------------------------------------- 6
section("6 worker 注册 / 认领 / 上报")
status, wk, _, _ = jcall("POST", "/api/v1/worker/register",
                         {"name": "smoke-worker", "capabilities": "paper|spigot"})
check(status == 201 and len(wk.get("worker_key", "")) > 10, "worker 注册成功")
wh = {"X-Worker-Key": (wk or {}).get("worker_key", "")}

claim = None
for _ in range(10):
    sclaim, c, _, _ = jcall("POST", "/api/v1/worker/claim", {}, wh)
    if sclaim == 204 or not c:
        break
    if c.get("task_id") == task_id:
        claim = c
        break
    # 队列里可能有历史遗留任务：清掉，避免干扰本次断言。
    multipart("/api/v1/worker/tasks/%d/result" % c["task_id"],
              {"status": "failed", "stage": "cleanup", "error": "smoke cleanup"}, {}, wh)
check(claim is not None, "worker 认领到目标任务")

if claim:
    tid = claim["task_id"]
    sres, raw = multipart("/api/v1/worker/tasks/%d/result" % tid,
                          {"status": "done", "pass_at_1": "1", "fix_rounds": "0", "ai_tokens": "1234"},
                          {"jar": ("plugin.jar", bytes(range(64)))}, wh)
    res = json.loads(raw.decode()) if raw else {}
    check(sres == 200 and res.get("status") == "done", "worker 上报完成")

    status, t, _, _ = jcall("GET", "/api/v1/tasks/%d" % tid, headers=uh)
    check(status == 200 and t.get("status") == "done", "任务状态 done")
    check(t.get("pass_at_1") == 1, "pass_at_1 已记录")
    check(t.get("download_url"), "返回签名下载地址")

    if t.get("download_url"):
        st2, body, hd = call("GET", t["download_url"])
        check(st2 == 200 and len(body) > 0, "签名地址可下载产物")

    status, me_after, _, _ = jcall("GET", "/api/me", headers=uh)
    check(me_after.get("signup_granted") == 1, "首单成功后标记已发放")
    check(me_after.get("balance") == 100, "首单赠送 +20（余额 100）")

# ---------------------------------------------------------------- 7
section("7 市场发布 / 购买")
status, prod, _, _ = jcall("POST", "/api/market/publish",
                           {"task_id": task_id, "title": "抽奖插件", "description": "smoke", "price": 0}, uh)
check(status == 201 and prod.get("product_id", 0) > 0, "发布商品")
pid = (prod or {}).get("product_id", 0)

status, dup, _, _ = jcall("POST", "/api/market/publish",
                          {"task_id": task_id, "title": "重复发布", "price": 0}, uh)
check(status == 409, "同一任务不能重复发布")

suffix2 = uuid.uuid4().hex[:8]
status, buyer, _, _ = jcall("POST", "/api/register",
                            {"email": "b_%s@example.com" % suffix2, "username": "b_" + suffix2,
                             "password": password})
bh = {"Authorization": "Bearer " + (buyer or {}).get("token", "")}
status, order, _, _ = jcall("POST", "/api/market/buy", {"product_id": pid}, bh)
check(status == 201 and order.get("ok") is True, "购买成功")

st3, dl_body, _ = call("GET", "/api/market/download?product_id=%d" % pid, headers=bh)
check(st3 == 200 and len(dl_body) > 0, "买家可下载")

# ---------------------------------------------------------------- 8
section("8 订阅")
status, sub, _, _ = jcall("POST", "/api/v1/subscriptions",
                          {"task_id": task_id, "target_versions": ["1.21.1"]}, uh)
check(status == 201 and sub.get("id", 0) > 0, "创建订阅")

status, dup_sub, _, _ = jcall("POST", "/api/v1/subscriptions",
                              {"task_id": task_id, "target_versions": ["1.21.1"]}, uh)
check(status == 409, "同一任务不能重复订阅")

status, subs, _, _ = jcall("GET", "/api/v1/subscriptions", headers=uh)
check(status == 200 and len(subs.get("items", [])) >= 1, "订阅列表返回")

# ---------------------------------------------------------------- 9
section("9 后台统计 / 审计 / 配置热更新")
status, dash, _, _ = jcall("GET", "/api/admin/dashboard", headers=ah)
check(status == 200 and dash.get("users", 0) >= 2, "dashboard 用户数正确")
check(dash.get("done", 0) >= 1, "dashboard 完成任务数 >= 1")
check(dash.get("config_version", 0) > 0, "dashboard 带配置版本")

status, audit, _, _ = jcall("GET", "/api/admin/audit", headers=ah)
check(status == 200 and len(audit.get("items", [])) >= 1, "审计日志有记录")

status, cur_cfg, _, _ = jcall("GET", "/api/config")
cur_versions = list((cur_cfg or {}).get("catalog", {}).get("mc_versions") or [])
new_version = "1.21.%d" % (int(uuid.uuid4().hex[:2], 16) % 90 + 10)
while new_version in cur_versions:
    new_version = "1.21.%d" % (int(uuid.uuid4().hex[:2], 16) % 90 + 10)

status, cfg, _, _ = jcall("POST", "/api/admin/config",
                          {"section": "catalog",
                           "values": {"mc_versions": cur_versions + [new_version]}}, ah)
check(status == 200 and cfg.get("config_version", 0) > pub.get("config_version", 0), "改配置后版本号递增")
check(len(cfg.get("added_versions") or []) == 1, "识别出新增版本 1 个（%s）" % new_version)
check(cfg.get("adapt_tasks", 0) >= 1, "为订阅自动生成适配任务")

# ---------------------------------------------------------------- 10
section("10 订阅列表 + 任务列表")
status, tl, _, _ = jcall("GET", "/api/v1/tasks?limit=10", headers=uh)
check(status == 200 and len(tl.get("items", [])) >= 2, "任务列表返回")

status, ov, _, _ = jcall("GET", "/api/v1/me/overview", headers=uh)
check(status == 200 and ov.get("user", {}).get("id", 0) > 0, "用户中心聚合返回")

# ---------------------------------------------------------------- 11
section("11 失败退款 / 免费重试 / 越权")

suffix3 = uuid.uuid4().hex[:8]
status, u3, _, _ = jcall("POST", "/api/register",
                         {"email": "f_%s@example.com" % suffix3, "username": "f_" + suffix3,
                          "password": password})
u3h = {"Authorization": "Bearer " + (u3 or {}).get("token", "")}
status, cdk3, _, _ = jcall("POST", "/api/admin/cdks", {"count": 1, "amount": 100, "batch": "fail-smoke"}, ah)
status, rd3, _, _ = jcall("POST", "/api/redeem", {"code": cdk3["codes"][0]}, u3h)
check(rd3.get("balance") == 100, "失败流程用户余额 100")

status, g3, _, _ = jcall("POST", "/api/v1/site/ai/generate",
                         {"platform": "paper", "mc_version": "1.20.4", "requirement": "失败流程测试"},
                         {"X-Site-Key": sitkey, "Authorization": u3h["Authorization"]})
target = (g3 or {}).get("task_id")
check(g3.get("cost") == 10, "失败流程任务扣 10")

got = None
for _ in range(10):
    s, c, _, _ = jcall("POST", "/api/v1/worker/claim", {}, wh)
    if s == 204 or not c:
        break
    if c.get("task_id") == target:
        got = c
        break
    multipart("/api/v1/worker/tasks/%d/result" % c["task_id"],
              {"status": "failed", "stage": "cleanup", "error": "smoke cleanup"}, {}, wh)
check(got is not None, "worker 认领到目标任务")

if got:
    sres, raw = multipart("/api/v1/worker/tasks/%d/result" % target,
                          {"status": "failed", "stage": "build",
                           "error": "javac: cannot find symbol"}, {}, wh)
    res = json.loads(raw.decode()) if raw else {}
    check(sres == 200 and res.get("status") == "failed", "上报失败")

    status, t3, _, _ = jcall("GET", "/api/v1/tasks/%d" % target, headers=u3h)
    check(t3.get("status") == "failed", "任务状态 failed")
    check(t3.get("refunded") is True, "标记已退款")
    check(t3.get("fail_stage") == "build", "失败阶段被记录")

    status, m3, _, _ = jcall("GET", "/api/me", headers=u3h)
    check(m3.get("balance") == 99, "失败只扣 1 积分（100-10+9）")

    status, rt, _, _ = jcall("POST", "/api/v1/tasks/%d/retry" % target, {}, u3h)
    check(status == 201 and rt.get("cost") == 0, "免费重试生成新任务")
    if rt and rt.get("task_id"):
        status, t4, _, _ = jcall("GET", "/api/v1/tasks/%d" % rt["task_id"], headers=u3h)
        check(t4.get("status") == "pending", "重试任务重新入队")

status, _, _, _ = jcall("GET", "/api/v1/tasks/%d" % task_id, headers=bh)
check(status == 403, "他人任务访问返回 403")

status, _, _, _ = jcall("GET", "/api/admin/dashboard", headers=uh)
check(status == 403, "普通用户访问后台返回 403")

print("\n========================================")
print("通过 %d 项，失败 %d 项" % (passed, failed))
sys.exit(1 if failed else 0)
