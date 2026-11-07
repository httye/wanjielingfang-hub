"""验证 events outbox → webhook 消费者链路：签名、事件类型、重试语义。

自包含：自己起一个本地 webhook 接收器 + 自己起 hub 子进程，跑完清理。
用法: python verify_webhook.py
"""

import hashlib
import hmac
import http.server
import json
import os
import shutil
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import uuid

HUB_DIR = os.path.dirname(os.path.abspath(__file__))
HUB_DIR = os.path.dirname(HUB_DIR) if os.path.basename(HUB_DIR) == "tools" else HUB_DIR
HUB_EXE = os.path.join(HUB_DIR, "hub.exe")
DATA_DIR = os.path.join(HUB_DIR, "data-webhook")
HUB_PORT = 18100
HOOK_PORT = 18099
BASE = "http://127.0.0.1:%d" % HUB_PORT

received = []
lock = threading.Lock()


class Hook(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length)
        with lock:
            received.append({
                "event": self.headers.get("X-MCAI-Event"),
                "ts": int(self.headers.get("X-MCAI-Timestamp", "0") or 0),
                "sig": self.headers.get("X-MCAI-Signature", ""),
                "body": body,
            })
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")

    def log_message(self, *args):
        pass


def wait_port(port, timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=1):
                return True
        except OSError:
            time.sleep(0.2)
    return False


def call(method, path, body=None, headers=None, timeout=30):
    data = json.dumps(body).encode() if body is not None else None
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def jcall(method, path, body=None, headers=None):
    s, raw = call(method, path, body, headers)
    try:
        return s, (json.loads(raw.decode()) if raw else None)
    except Exception:
        return s, None


def multipart(path, fields, files, headers):
    boundary = "----wh" + uuid.uuid4().hex
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
        print("找不到 hub.exe，先在 hub 目录执行 go build -o hub.exe ./cmd/hub")
        return 1
    shutil.rmtree(DATA_DIR, ignore_errors=True)

    srv = http.server.HTTPServer(("127.0.0.1", HOOK_PORT), Hook)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    print("webhook 接收器启动 :%d" % HOOK_PORT)

    env = dict(os.environ)
    env.update({
        "PORT": str(HUB_PORT),
        "JWT_SECRET": "webhook-test-secret-0123456789",
        "ADMIN_PASSWORD": "admin123",
        "DATA_DIR": DATA_DIR,
        "AI_MOCK": "1",
    })
    proc = subprocess.Popen([HUB_EXE], cwd=HUB_DIR, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    passed = failed = 0

    def check(cond, msg):
        nonlocal passed, failed
        if cond:
            passed += 1
            print("  PASS " + msg)
        else:
            failed += 1
            print("  FAIL " + msg)

    try:
        if not wait_port(HUB_PORT):
            print("hub 未启动")
            return 1
        time.sleep(1)

        print("\n== 准备站点与任务 ==")
        sfx = uuid.uuid4().hex[:8]
        s, reg = jcall("POST", "/api/register",
                       {"email": "wh_%s@example.com" % sfx, "username": "wh_" + sfx,
                        "password": "Passw0rd!123"})
        uh = {"Authorization": "Bearer " + reg["token"]}

        s, adm = jcall("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
        ah = {"Authorization": "Bearer " + adm["token"]}
        s, cdk = jcall("POST", "/api/admin/cdks", {"count": 1, "amount": 100, "batch": "wh"}, ah)
        jcall("POST", "/api/redeem", {"code": cdk["codes"][0]}, uh)

        hook_url = "http://127.0.0.1:%d/hook" % HOOK_PORT
        s, st = jcall("POST", "/api/sites",
                      {"name": "wh-site", "base_url": "https://wh.example.com", "webhook_url": hook_url}, uh)
        check(s == 201, "站点创建（含 webhook_url）")
        sitkey = st["site_key"]
        secret = st["webhook_secret"]
        check(len(secret) > 10, "返回 webhook_secret")

        s, gen = jcall("POST", "/api/v1/site/ai/generate",
                       {"platform": "paper", "mc_version": "1.20.4", "requirement": "webhook 验证"},
                       {"X-Site-Key": sitkey, "Authorization": uh["Authorization"]})
        check(s == 201, "提交生成任务")

        s, wk = jcall("POST", "/api/v1/worker/register", {"name": "wh-worker", "capabilities": "paper"})
        wh = {"X-Worker-Key": wk["worker_key"]}
        target = gen["task_id"]
        got = None
        for _ in range(6):
            s, c = jcall("POST", "/api/v1/worker/claim", {}, wh)
            if s == 204 or not c:
                break
            if c.get("task_id") == target:
                got = c
                break
            multipart("/api/v1/worker/tasks/%d/result" % c["task_id"],
                      {"status": "failed", "stage": "cleanup", "error": "cleanup"}, {}, wh)
        check(got is not None, "worker 认领到目标任务")

        print("\n== 上报完成，等待 outbox 投递 ==")
        multipart("/api/v1/worker/tasks/%d/result" % target,
                  {"status": "done", "pass_at_1": "1", "fix_rounds": "0", "ai_tokens": "42"},
                  {"jar": ("p.jar", bytes(range(32)))}, wh)

        deadline = time.time() + 15
        while time.time() < deadline:
            with lock:
                if any(r["event"] == "task.done" for r in received):
                    break
            time.sleep(0.3)

        with lock:
            events = [r["event"] for r in received]
            snapshot = list(received)
        check("task.created" in events, "收到 task.created 事件")
        check("task.done" in events, "收到 task.done 事件")

        ok_sig = 0
        bad_sig = 0
        for r in snapshot:
            expect = hmac.new(secret.encode(), str(r["ts"]).encode() + b"." + r["body"],
                              hashlib.sha256).hexdigest()
            if hmac.compare_digest(expect, r["sig"]):
                ok_sig += 1
            else:
                bad_sig += 1
        check(ok_sig >= 2 and bad_sig == 0, "全部回调签名校验通过（%d 条）" % ok_sig)

        done = [r for r in snapshot if r["event"] == "task.done"]
        payload_ok = False
        if done:
            try:
                body = json.loads(done[0]["body"].decode())
                payload_ok = body.get("payload", {}).get("task_id") == target
            except Exception:
                payload_ok = False
        check(payload_ok, "task.done 载荷含正确 task_id")

        # 未注册站点不应收到回调：再造一个没有 webhook 的站点提交任务
        print("\n== 无 webhook 的站点不产生回调 ==")
        s, st2 = jcall("POST", "/api/sites", {"name": "no-hook", "base_url": "https://nh.example.com"}, uh)
        s, gen2 = jcall("POST", "/api/v1/site/ai/generate",
                        {"platform": "paper", "mc_version": "1.20.4", "requirement": "无回调"},
                        {"X-Site-Key": st2["site_key"], "Authorization": uh["Authorization"]})
        check(s == 201, "无 webhook 站点也能提交任务")

    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        srv.shutdown()
        shutil.rmtree(DATA_DIR, ignore_errors=True)

    print("\n========================================")
    print("通过 %d 项，失败 %d 项" % (passed, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
