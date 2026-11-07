"""验证站点级计费：站长改价、下限保护、按站扣费、下发一致性。"""
import json
import os
import shutil
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

HERE = os.path.dirname(os.path.abspath(__file__))
HUB_DIR = os.path.dirname(HERE) if os.path.basename(HERE) == "tools" else HERE
HUB_EXE = os.path.join(HUB_DIR, "hub.exe")
DATA_DIR = os.path.join(HUB_DIR, "data-billing")
PORT = 18104
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


def call(method, path, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def jcall(method, path, body=None, headers=None):
    s, raw = call(method, path, body, headers)
    try:
        return s, (json.loads(raw.decode()) if raw else None)
    except Exception:
        return s, None


def wait_port(port, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def main():
    if not os.path.exists(HUB_EXE):
        print("找不到 hub.exe")
        return 1
    shutil.rmtree(DATA_DIR, ignore_errors=True)
    env = dict(os.environ)
    env.update({
        "PORT": str(PORT), "JWT_SECRET": "billing-test-0123456789",
        "ADMIN_PASSWORD": "admin123", "DATA_DIR": DATA_DIR, "AI_MOCK": "1",
    })
    proc = subprocess.Popen([HUB_EXE], cwd=HUB_DIR, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_port(PORT):
            print("hub 未启动")
            return 1
        time.sleep(1)

        sfx = uuid.uuid4().hex[:8]
        s, reg = jcall("POST", "/api/register",
                       {"email": "bill_%s@example.com" % sfx, "username": "bill_" + sfx,
                        "password": "Passw0rd!123"})
        uh = {"Authorization": "Bearer " + reg["token"]}
        s, adm = jcall("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
        ah = {"Authorization": "Bearer " + adm["token"]}
        s, cdk = jcall("POST", "/api/admin/cdks", {"count": 1, "amount": 500, "batch": "bill"}, ah)
        jcall("POST", "/api/redeem", {"code": cdk["codes"][0]}, uh)
        # 本用例会连提多个任务，放宽并发上限
        jcall("POST", "/api/admin/config", {"section": "limits", "values": {"max_active_tasks": 10}}, ah)

        print("\n== 两个站点：一个自定义价、一个走默认 ==")
        s, st1 = jcall("POST", "/api/sites", {"name": "站A", "base_url": "https://a.example.com"}, uh)
        s, st2 = jcall("POST", "/api/sites", {"name": "站B", "base_url": "https://b.example.com"}, uh)
        check(s == 201, "创建两个站点")

        s, r = jcall("PUT", "/api/sites/%d/override" % st1["id"],
                     {"override": json.dumps({"billing": {"task_cost": 3, "iterate_cost": 2,
                                                          "credit_cost_fail": 0}})}, uh)
        check(s == 200, "站A 设置自定义计费")

        print("\n== 下限保护 ==")
        s, r = jcall("PUT", "/api/sites/%d/override" % st1["id"],
                     {"override": json.dumps({"billing": {"task_cost": 0}})}, uh)
        check(s == 400, "低于下限被拒（400）")
        s, r = jcall("PUT", "/api/sites/%d/override" % st1["id"],
                     {"override": json.dumps({"billing": {"signup_bonus": 9999}})}, uh)
        check(s == 400, "不允许改 signup_bonus（400）")
        s, r = jcall("PUT", "/api/sites/%d/override" % st1["id"],
                     {"override": json.dumps({"features": {"market": False}})}, uh)
        check(s == 400, "不允许覆盖 features（400）")

        print("\n== 实际扣费按站点价 ==")
        s, g1 = jcall("POST", "/api/v1/site/ai/generate",
                      {"platform": "paper", "mc_version": "1.20.4", "requirement": "站A任务"},
                      {"X-Site-Key": st1["site_key"], "Authorization": uh["Authorization"]})
        check(s == 201 and g1["cost"] == 3, "站A 生成任务扣 3 积分")

        s, g2 = jcall("POST", "/api/v1/site/ai/generate",
                      {"platform": "paper", "mc_version": "1.20.4", "requirement": "站B任务"},
                      {"X-Site-Key": st2["site_key"], "Authorization": uh["Authorization"]})
        check(s == 201 and g2["cost"] == 10, "站B 走平台默认价 10 积分")

        print("\n== 计费规则不影响其它站点 ==")
        s, cfg2 = jcall("GET", "/api/v1/site/config", headers={"X-Site-Key": st2["site_key"]})
        check(s == 200 and cfg2["billing"]["task_cost"] == 10, "站B 下发配置仍是默认价")

        s, cfg1 = jcall("GET", "/api/v1/site/config", headers={"X-Site-Key": st1["site_key"]})
        check(s == 200 and cfg1["billing"]["task_cost"] == 3, "站A 下发配置是自己设的价")

        print("\n== 清除自定义价恢复默认 ==")
        s, r = jcall("PUT", "/api/sites/%d/override" % st1["id"], {"override": ""}, uh)
        check(s == 200, "清空 override")
        s, g3 = jcall("POST", "/api/v1/site/ai/generate",
                      {"platform": "paper", "mc_version": "1.20.4", "requirement": "恢复默认"},
                      {"X-Site-Key": st1["site_key"], "Authorization": uh["Authorization"]})
        check(s == 201 and g3["cost"] == 10, "站A 恢复默认价 10 积分")

        print("\n== 保留 ui/brand 覆盖不丢 ==")
        s, r = jcall("PUT", "/api/sites/%d/override" % st2["id"],
                     {"override": json.dumps({"ui": {"promotion": "活动"},
                                              "billing": {"task_cost": 7}})}, uh)
        check(s == 200, "同时设置 ui 与 billing")
        s, ov = jcall("GET", "/api/sites/%d/override" % st2["id"], headers=uh)
        obj = json.loads(ov["override"])
        check(obj["ui"]["promotion"] == "活动" and obj["billing"]["task_cost"] == 7, "两部分都保留")

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
