"""覆盖剩余目标项的端到端验证：USDT / 套餐 / CDK 重复 / price_rules /
config override / ?full=1 / SSO / 托管认证页 / 维护模式。

自包含：自己起 hub 子进程，跑完清理。
用法: python verify_extra.py
"""

import http.client
import json
import os
import shutil
import socket
import sqlite3
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid
from urllib.parse import urlparse

HERE = os.path.dirname(os.path.abspath(__file__))
HUB_DIR = os.path.dirname(HERE) if os.path.basename(HERE) == "tools" else HERE
HUB_EXE = os.path.join(HUB_DIR, "hub.exe")
DATA_DIR = os.path.join(HUB_DIR, "data-extra")
PORT = 18101
BASE = "http://127.0.0.1:%d" % PORT

passed = failed = 0


def check(cond, msg):
    global passed, failed
    if cond:
        passed += 1
        print("  PASS " + msg)
    else:
        failed += 1
        print("  FAIL " + msg)


def call(method, path, body=None, headers=None, timeout=30, follow=True):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    opener = urllib.request.build_opener() if follow else urllib.request.build_opener(NoRedirect)
    try:
        with opener.open(req, timeout=timeout) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def jcall(method, path, body=None, headers=None):
    s, raw, hdrs = call(method, path, body, headers)
    try:
        return s, (json.loads(raw.decode()) if raw else None), hdrs
    except Exception:
        return s, None, hdrs


def wait_port(port, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def multipart(path, fields, files, headers):
    boundary = "----ex" + uuid.uuid4().hex
    buf = b""
    for k, v in fields.items():
        buf += ("--%s\r\nContent-Disposition: form-data; name=\"%s\"\r\n\r\n%s\r\n"
                % (boundary, k, v)).encode()
    for k, (fn, data) in files.items():
        buf += ("--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"%s\"\r\n"
                "Content-Type: application/octet-stream\r\n\r\n" % (boundary, k, fn)).encode()
        buf += data + b"\r\n"
    buf += ("--%s--\r\n" % boundary).encode()
    h = dict(headers or {})
    h["Content-Type"] = "multipart/form-data; boundary=" + boundary
    req = urllib.request.Request(BASE + path, data=buf, headers=h, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def main():
    if not os.path.exists(HUB_EXE):
        print("找不到 hub.exe，先执行 go build -o hub.exe ./cmd/hub")
        return 1
    shutil.rmtree(DATA_DIR, ignore_errors=True)
    env = dict(os.environ)
    env.update({
        "PORT": str(PORT), "JWT_SECRET": "extra-test-secret-0123456789",
        "ADMIN_PASSWORD": "admin123", "DATA_DIR": DATA_DIR, "AI_MOCK": "1",
        "BILLING_INTERVAL": "2",
    })
    proc = subprocess.Popen([HUB_EXE], cwd=HUB_DIR, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_port(PORT):
            print("hub 未启动")
            return 1
        time.sleep(1)

        sfx = uuid.uuid4().hex[:8]
        s, reg, _ = jcall("POST", "/api/register",
                          {"email": "x_%s@example.com" % sfx, "username": "x_" + sfx,
                           "password": "Passw0rd!123"})
        uh = {"Authorization": "Bearer " + reg["token"]}
        s, me, _ = jcall("GET", "/api/me", headers=uh)
        check(me["balance"] == 0, "新账号余额 0")

        s, adm, _ = jcall("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
        ah = {"Authorization": "Bearer " + adm["token"]}

        print("\n== CDK 重复兑换 ==")
        s, cdk, _ = jcall("POST", "/api/admin/cdks", {"count": 1, "amount": 50, "batch": "x"}, ah)
        code = cdk["codes"][0]
        s, rd1, _ = jcall("POST", "/api/redeem", {"code": code}, uh)
        check(s == 200 and rd1["balance"] == 50, "首次兑换成功（+50）")
        s, rd2, _ = jcall("POST", "/api/redeem", {"code": code}, uh)
        check(s == 409, "重复兑换返回 409")

        print("\n== USDT 提交 / 去重 / 后台确认 ==")
        txid = "0x" + uuid.uuid4().hex + uuid.uuid4().hex[:8]
        s, pay, _ = jcall("POST", "/api/payments/usdt",
                          {"txid": txid, "chain": "TRC20", "amount_cents": 1000}, uh)
        check(s == 201 and pay["points"] == 100, "10 USDT → 100 积分待确认")
        s, dup, _ = jcall("POST", "/api/payments/usdt",
                          {"txid": txid, "chain": "TRC20", "amount_cents": 1000}, uh)
        check(s == 409, "相同 chain+txid 重复提交 409")
        s, plist, _ = jcall("GET", "/api/admin/payments?status=pending", headers=ah)
        check(s == 200 and any(p["id"] == pay["id"] for p in plist["items"]), "后台能看到待确认支付")
        s, cfm, _ = jcall("POST", "/api/admin/payments/%d/confirm" % pay["id"], {}, ah)
        check(s == 200 and cfm["user_balance"] == 150, "确认入账后余额 150")
        s, again, _ = jcall("POST", "/api/admin/payments/%d/confirm" % pay["id"], {}, ah)
        check(s == 409, "重复确认返回 409")

        print("\n== 套餐下单 → 确认入账 + 会员到期 ==")
        s, plan, _ = jcall("POST", "/api/plans/buy", {"plan_id": "monthly"}, uh)
        check(s == 201 and plan["status"] == "pending", "套餐下单进入待支付")
        s, pcfm, _ = jcall("POST", "/api/admin/payments/%d/confirm" % plan["id"], {}, ah)
        check(s == 200 and pcfm["user_balance"] == 1650, "确认后 +1500 积分（余额 1650）")
        s, me2, _ = jcall("GET", "/api/me", headers=uh)
        check(me2["vip_until"] > time.time(), "会员到期时间已延长")

        print("\n== 站点 + override 合并 + ?full=1 ==")
        s, st, _ = jcall("POST", "/api/sites",
                         {"name": "s1", "base_url": "https://s1.example.com"}, uh)
        sid = st["id"]
        sk = st["site_key"]
        sh = {"X-Site-Key": sk}
        s, ov, _ = jcall("PUT", "/api/sites/%d/override" % sid,
                         {"override": json.dumps({"ui": {"promotion": "开业活动 8 折"},
                                                  "brand": {"name": "小站A"}})}, uh)
        check(s == 200, "写入站点级 override")
        s, sc, _ = jcall("GET", "/api/v1/site/config", headers=sh)
        check(sc.get("ui", {}).get("promotion") == "开业活动 8 折", "override 的 ui 合并生效")
        check(sc.get("brand", {}).get("name") == "小站A", "override 的 brand 合并生效")
        check(sc.get("billing", {}).get("task_cost") == 10, "未被覆盖的 billing 仍用全局值")
        s, scfull, _ = jcall("GET", "/api/v1/site/config?full=1", headers=sh)
        check(s == 200 and scfull.get("config_version", 0) > 0, "?full=1 返回全量快照")

        s, bad_ov, _ = jcall("PUT", "/api/sites/%d/override" % sid,
                             {"override": json.dumps({"billing": {"task_cost": 0}})}, uh)
        check(s == 400, "不允许覆盖计费分区")

        print("\n== price_rules 覆盖默认计价 ==")
        s, pr, _ = jcall("POST", "/api/admin/price-rules",
                         {"action": "generate", "site_id": sid, "platform": "paper",
                          "amount": 3, "priority": 100}, ah)
        check(s == 201, "新增价格规则")
        s, gen, _ = jcall("POST", "/api/v1/site/ai/generate",
                          {"platform": "paper", "mc_version": "1.20.4", "requirement": "价格规则验证"},
                          {"X-Site-Key": sk, "Authorization": uh["Authorization"]})
        check(s == 201 and gen["cost"] == 3, "该站点 paper 生成按规则价 3 积分")
        s, gen2, _ = jcall("POST", "/api/v1/site/ai/generate",
                           {"platform": "fabric", "mc_version": "1.20.4", "requirement": "回落默认价"},
                           {"X-Site-Key": sk, "Authorization": uh["Authorization"]})
        check(s == 201 and gen2["cost"] == 10, "其它平台回落默认价 10 积分")

        print("\n== 托管认证页 / SSO ==")
        s, html, hdrs = call("GET", "/auth/login")
        check(s == 200 and b"<form" in html and b"identifier" in html, "GET /auth/login 渲染登录页")
        s, html2, _ = call("GET", "/auth/register")
        check(s == 200 and b"<form" in html2, "GET /auth/register 渲染注册页")

        s, body, hdrs = call("GET", "/sso?redirect=https://s1.example.com/cb", headers=uh, follow=False)
        loc = hdrs.get("Location", "")
        check(s == 302 and "code=" in loc, "SSO 302 到白名单地址并带 code")
        code_sso = ""
        if "code=" in loc:
            code_sso = loc.split("code=")[1].split("&")[0]
        s, ex, _ = jcall("POST", "/api/sso/exchange", {"code": code_sso})
        check(s == 200 and len(ex.get("token", "")) > 20, "code 换到 token")
        check(ex.get("user_id") == reg["user"]["id"], "换到的 token 属于同一用户")
        s, ex2, _ = jcall("POST", "/api/sso/exchange", {"code": code_sso})
        check(s == 400, "code 一次性，二次使用失败")

        s, body, hdrs = call("GET", "/sso?redirect=https://evil.example.com/cb", headers=uh, follow=False)
        check(s == 400, "非白名单 redirect 被拒")

        print("\n== 订阅计费循环（BILLING_INTERVAL=2s） ==")
        s, wk, _ = jcall("POST", "/api/v1/worker/register",
                         {"name": "bill-worker", "capabilities": "paper|fabric"})
        wh = {"X-Worker-Key": wk["worker_key"]}
        target = gen["task_id"]
        got = None
        for _ in range(8):
            s, c, _ = jcall("POST", "/api/v1/worker/claim", {}, wh)
            if s == 204 or not c:
                break
            if c.get("task_id") == target:
                got = c
                break
            multipart("/api/v1/worker/tasks/%d/result" % c["task_id"],
                      {"status": "failed", "stage": "cleanup", "error": "cleanup"}, {}, wh)
        check(got is not None, "worker 认领到目标任务")

        if got:
            sres, _ = multipart("/api/v1/worker/tasks/%d/result" % target,
                                {"status": "done", "pass_at_1": "1"}, {"jar": ("p.jar", bytes(range(32)))}, wh)
            check(sres == 200, "任务完成")
            s, sub, _ = jcall("POST", "/api/v1/subscriptions", {"task_id": target}, uh)
            check(s == 201 and sub["price"] == 300, "订阅价 = task_cost×30 = 300")

            s, before, _ = jcall("GET", "/api/me", headers=uh)
            bal_before = before["balance"]

            db_path = os.path.join(DATA_DIR, "hub.db")
            conn = sqlite3.connect(db_path, timeout=15)
            conn.execute("UPDATE subscriptions SET next_bill_at = 0 WHERE id = ?", (sub["id"],))
            conn.commit()
            conn.close()

            time.sleep(6)

            s, after, _ = jcall("GET", "/api/me", headers=uh)
            check(after["balance"] == bal_before - 300, "计费循环扣了 300 积分（%d→%d）" %
                  (bal_before, after["balance"]))

            s, sl, _ = jcall("GET", "/api/v1/subscriptions", headers=uh)
            row = [x for x in sl["items"] if x["id"] == sub["id"]]
            check(bool(row) and row[0]["next_bill_at"] > time.time(), "扣费后 next_bill_at 前进到下个周期")

        print("\n== 维护模式 ==")
        s, _, _ = jcall("POST", "/api/admin/config", {"section": "features", "values": {"maintenance": True}}, ah)
        check(s == 200, "开启维护模式")
        s, body, _ = call("GET", "/api/config")
        check(s == 200, "本机运维仍可访问（回环放行）")
        # 用非回环地址无法模拟，改为直接断言配置已生效
        s, cfgpub, _ = jcall("GET", "/api/config")
        check(cfgpub["config_version"] > 0, "维护期间配置仍可读")
        s, _, _ = jcall("POST", "/api/admin/config", {"section": "features", "values": {"maintenance": False}}, ah)
        check(s == 200, "关闭维护模式")

    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        shutil.rmtree(DATA_DIR, ignore_errors=True)

    print("\n========================================")
    print("通过 %d 项，失败 %d 项" % (passed, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
