"""验证 AI 中转网关：OpenAI 兼容入口、渠道故障转移、用量统计、API key 鉴权。

自包含：起假上游 + 起 hub 子进程，跑完清理。
用法: python verify_ai.py
"""

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

HERE = os.path.dirname(os.path.abspath(__file__))
HUB_DIR = os.path.dirname(HERE) if os.path.basename(HERE) == "tools" else HERE
HUB_EXE = os.path.join(HUB_DIR, "hub.exe")
DATA_DIR = os.path.join(HUB_DIR, "data-ai")
HUB_PORT = 18102
UP_PORT = 18098
BASE = "http://127.0.0.1:%d" % HUB_PORT

seen = []
passed = failed = 0


def check(cond, msg):
    global passed, failed
    if cond:
        passed += 1
        print("  PASS " + msg)
    else:
        failed += 1
        print("  FAIL " + msg)


class Upstream(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        req = json.loads(body)
        seen.append({
            "path": self.path,
            "model": req.get("model"),
            "stream": bool(req.get("stream")),
            "auth": self.headers.get("Authorization", ""),
        })
        if req.get("stream"):
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            for tok in ["你好", "，", "世界"]:
                chunk = {"choices": [{"delta": {"content": tok}}]}
                self.wfile.write(("data: " + json.dumps(chunk, ensure_ascii=False) + "\n\n").encode())
                self.wfile.flush()
            self.wfile.write(('data: ' + json.dumps({"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}}) + "\n\n").encode())
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        else:
            resp = {
                "id": "chatcmpl-mock", "object": "chat.completion", "model": req.get("model"),
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "你好，世界"},
                             "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 12, "completion_tokens": 8, "total_tokens": 20},
            }
            data = json.dumps(resp, ensure_ascii=False).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    def log_message(self, *a):
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


def call(method, path, body=None, headers=None, timeout=40, raw_body=None):
    data = raw_body if raw_body is not None else (json.dumps(body).encode() if body is not None else None)
    h = {"Content-Type": "application/json"}
    if headers:
        h.update(headers)
    req = urllib.request.Request(BASE + path, data=data, headers=h, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            return r.status, r.read(), dict(r.headers)
    except urllib.error.HTTPError as e:
        return e.code, e.read(), dict(e.headers)
    except ConnectionResetError:
        return 0, b"", {}


def jcall(method, path, body=None, headers=None):
    s, raw, hdrs = call(method, path, body, headers)
    try:
        return s, (json.loads(raw.decode()) if raw else None), hdrs
    except Exception:
        return s, None, hdrs


def main():
    if not os.path.exists(HUB_EXE):
        print("找不到 hub.exe")
        return 1
    shutil.rmtree(DATA_DIR, ignore_errors=True)

    up = http.server.HTTPServer(("127.0.0.1", UP_PORT), Upstream)
    threading.Thread(target=up.serve_forever, daemon=True).start()
    print("假上游启动 :%d" % UP_PORT)

    env = dict(os.environ)
    env.update({
        "PORT": str(HUB_PORT), "JWT_SECRET": "ai-gate-secret-0123456789",
        "ADMIN_PASSWORD": "admin123", "DATA_DIR": DATA_DIR,
        # 必须让 Mock=false，否则请求会走内置桩、打不到假上游
        "AI_BASE_URL": "", "AI_API_KEY": "sk-dummy", "AI_MOCK": "0",
    })
    proc = subprocess.Popen([HUB_EXE], cwd=HUB_DIR, env=env,
                            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    try:
        if not wait_port(HUB_PORT):
            print("hub 未启动")
            return 1
        time.sleep(1)

        sfx = uuid.uuid4().hex[:8]
        s, reg, _ = jcall("POST", "/api/register",
                          {"email": "ai_%s@example.com" % sfx, "username": "ai_" + sfx,
                           "password": "Passw0rd!123"})
        uh = {"Authorization": "Bearer " + reg["token"]}
        s, adm, _ = jcall("POST", "/api/login", {"identifier": "admin", "password": "admin123"})
        ah = {"Authorization": "Bearer " + adm["token"]}

        print("\n== 渠道配置 ==")
        s, ch, _ = jcall("POST", "/api/admin/ai/channels",
                         {"name": "mock-primary", "base_url": "http://127.0.0.1:%d/v1" % UP_PORT,
                          "api_key": "sk-upstream-test", "model": "mock-large", "priority": 50}, ah)
        check(s == 201, "新增主渠道")
        # 故意加一个连不上的高优先级渠道，验证故障转移
        s, bad, _ = jcall("POST", "/api/admin/ai/channels",
                          {"name": "dead-channel", "base_url": "http://127.0.0.1:9/v1",
                           "api_key": "x", "model": "dead", "priority": 100}, ah)
        check(s == 201, "新增不可达渠道（高优先级）")

        s, listc, _ = jcall("GET", "/api/admin/ai/channels", headers=ah)
        check(s == 200 and len(listc["items"]) == 2, "渠道列表返回 2 条")
        check(all("••••" in (c["api_key"] or "••••") for c in listc["items"] if c["api_key"]), "渠道密钥被打码")

        print("\n== API Key ==")
        s, key, _ = jcall("POST", "/api/v1/ai/keys", {"name": "my-key"}, uh)
        check(s == 201 and key["api_key"].startswith("ak_"), "签发 AI key")
        ak = {"X-API-Key": key["api_key"]}

        s, keys, _ = jcall("GET", "/api/v1/ai/keys", headers=uh)
        check(s == 200 and keys["items"][0]["api_key"].endswith(key["api_key"][-4:]), "key 列表脱敏展示")

        print("\n== /v1/models ==")
        s, models, _ = jcall("GET", "/v1/models", headers=ak)
        ids = [m["id"] for m in (models or {}).get("data", [])]
        check(s == 200 and "mock-large" in ids, "模型列表含渠道模型")

        print("\n== 非流式对话（含故障转移） ==")
        body = {"model": "mock-large", "messages": [{"role": "user", "content": "你好"}]}
        s, resp, _ = jcall("POST", "/v1/chat/completions", body, ak)
        check(s == 200 and resp["choices"][0]["message"]["content"] == "你好，世界", "转发成功并返回上游内容")
        check(seen and seen[0]["auth"] == "Bearer sk-upstream-test", "上游收到渠道密钥（不是客户端 key）")
        check(seen and seen[-1]["model"] == "mock-large", "model 按渠道覆写")

        print("\n== 流式对话 ==")
        s, raw, hdrs = call("POST", "/v1/chat/completions",
                            {"model": "mock-large", "stream": True,
                             "messages": [{"role": "user", "content": "hi"}]}, ak)
        text = raw.decode("utf-8", "replace")
        check(s == 200 and "text/event-stream" in hdrs.get("Content-Type", ""), "返回 SSE")
        check("你好" in text and "[DONE]" in text, "增量与结束标记透传")

        time.sleep(0.5)
        print("\n== 用量统计 ==")
        s, usage, _ = jcall("GET", "/v1/usage", headers=ak)
        total = sum(i["total_tokens"] for i in (usage or {}).get("items", []))
        check(s == 200 and total >= 35, "用户用量统计 %d tokens（20 + 15）" % total)

        s, au, _ = jcall("GET", "/api/admin/ai/usage", headers=ah)
        check(s == 200 and au["calls"] >= 2, "后台统计调用次数 %d" % au["calls"])
        check(au["total_tokens"] >= 35, "后台统计 token 总数 %d" % au["total_tokens"])
        check(any(c["channel"] == "mock-primary" for c in au["by_channel"]), "按渠道聚合正确")

        print("\n== 鉴权 ==")
        s, _, _ = jcall("POST", "/v1/chat/completions", body, {"X-API-Key": "ak_bad_key"})
        check(s == 401, "错误 key 返回 401")
        s, _, _ = jcall("POST", "/v1/chat/completions", body, {})
        check(s == 401, "无凭据返回 401")

        s, _, _ = jcall("POST", "/api/v1/ai/keys/%d/revoke" % key["id"], {}, uh)
        check(s == 200, "吊销 key")
        s, _, _ = jcall("POST", "/v1/chat/completions", body, ak)
        check(s in (401, 404), "吊销后立即失效")

        print("\n== 用户 JWT 也能直接调 ==")
        s, resp2, _ = jcall("POST", "/v1/chat/completions", body, uh)
        check(s == 200, "登录态可直接调用中转")

    finally:
        proc.terminate()
        try:
            proc.wait(timeout=10)
        except Exception:
            proc.kill()
        up.shutdown()
        shutil.rmtree(DATA_DIR, ignore_errors=True)

    print("\n========================================")
    print("通过 %d 项，失败 %d 项" % (passed, failed))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
