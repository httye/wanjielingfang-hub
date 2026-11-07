"""提取 HTML 内联脚本并用 node --check 做语法校验。"""
import os
import re
import subprocess
import sys
import tempfile

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE) if os.path.basename(HERE) == "tools" else HERE
WEB = os.path.join(ROOT, "internal", "modules", "web")
NODE = r"C:\Users\uu\.dsh\dsh-runtimes\dsh-primary-runtime\dependencies\node\bin\node.exe"

targets = [os.path.join(WEB, "assets", "app.js")]
for name in ("console.html", "admin.html"):
    html = open(os.path.join(WEB, name), encoding="utf-8").read()
    blocks = re.findall(r"<script(?![^>]*\bsrc=)[^>]*>(.*?)</script>", html, re.S)
    for i, b in enumerate(blocks):
        targets.append((name, i, b))

ok = True
for t in targets:
    if isinstance(t, tuple):
        name, idx, code = t
        label = "%s[script#%d]" % (name, idx)
    else:
        label = os.path.basename(t)
        code = open(t, encoding="utf-8").read()

    fd, tmp = tempfile.mkstemp(suffix=".js")
    os.close(fd)
    with open(tmp, "w", encoding="utf-8") as f:
        f.write(code)
    proc = subprocess.run([NODE, "--check", tmp], capture_output=True, text=True)
    os.unlink(tmp)
    if proc.returncode == 0:
        print("  PASS %s 语法正确" % label)
    else:
        ok = False
        print("  FAIL %s\n%s" % (label, proc.stderr.strip()))

sys.exit(0 if ok else 1)
