"""跨包符号检查：确认 core./task./wallet./subs. 引用的符号真实存在，并核对 Deps 字段方法。"""
import os
import re

ROOT = r"C:\Users\uu\repos\mcai-platform\hub"
CORE = os.path.join(ROOT, "internal", "core")
MODS = os.path.join(ROOT, "internal", "modules")


def read(p):
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


def strip(src):
    src = re.sub(r"/\*.*?\*/", "", src, flags=re.S)
    src = re.sub(r"//[^\n]*", "", src)
    return src


def top_decls(paths):
    """返回 (values, methods) —— values: 包级符号；methods: (receiver_type, method)。"""
    values, methods = set(), set()
    for p in paths:
        src = strip(read(p))
        for m in re.finditer(r"^func\s+(\w+)\s*\(", src, re.M):
            values.add(m.group(1))
        for m in re.finditer(r"^func\s*\(\s*\w+\s+\*?(\w+)\s*\)\s*(\w+)\s*\(", src, re.M):
            methods.add((m.group(1), m.group(2)))
        for m in re.finditer(r"^type\s+(\w+)\b", src, re.M):
            values.add(m.group(1))
        for m in re.finditer(r"^var\s+(\w+)\b", src, re.M):
            values.add(m.group(1))
        # const 块
        for blk in re.finditer(r"^const\s*\((.*?)^\)", src, re.S | re.M):
            for line in blk.group(1).splitlines():
                mm = re.match(r"\s*(\w+)\s*=", line)
                if mm:
                    values.add(mm.group(1))
        for m in re.finditer(r"^const\s+(\w+)\s*=", src, re.M):
            values.add(m.group(1))
    return values, methods


def go_files(d):
    out = []
    for dirpath, _dirs, names in os.walk(d):
        for n in names:
            if n.endswith(".go"):
                out.append(os.path.join(dirpath, n))
    return out


core_values, core_methods = top_decls(go_files(CORE))
task_values, task_methods = top_decls(go_files(os.path.join(MODS, "task")))
wallet_values, _ = top_decls(go_files(os.path.join(MODS, "wallet")))
subs_values, _ = top_decls(go_files(os.path.join(MODS, "subs")))

print(f"core 导出符号 {len(core_values)} 个，方法 {len(core_methods)} 个")
print(f"task 导出 {len(task_values)}，wallet {len(wallet_values)}，subs {len(subs_values)}\n")

targets = {
    "core": core_values,
    "task": task_values,
    "wallet": wallet_values,
    "subs": subs_values,
}

# Deps 字段 → 类型
deps_fields = {
    "DB": "DB", "Auth": "Auth", "Cfg": "Config", "Ev": "Events",
    "Price": "Price", "Log": "Logger", "HTTP": "Client", "Env": "map",
}
method_map = {}
for t, meth in core_methods:
    method_map.setdefault(t, set()).add(meth)

problems = []
for p in go_files(os.path.join(ROOT, "cmd")) + go_files(MODS):
    rel = os.path.relpath(p, ROOT)
    src = strip(read(p))
    pkg = re.search(r"^package\s+(\w+)", src, re.M).group(1)
    for alias, sym in re.findall(r"\b(core|task|wallet|subs)\.(\w+)", src):
        pool = targets[alias]
        if sym not in pool:
            problems.append(f"{rel}: 引用了不存在的 {alias}.{sym}")
    # d.Cfg.Xxx / d.Auth.Xxx 方法检查
    for field, meth in re.findall(r"\bd\.(DB|Auth|Cfg|Ev|Price|Log|HTTP)\.(\w+)", src):
        typ = deps_fields[field]
        if typ not in method_map:
            continue
        if meth not in method_map[typ] and meth not in {
            # 标准库方法，跳过
            "Printf", "Do", "Exec", "Query", "QueryRow", "BeginTx",
        }:
            problems.append(f"{rel}: d.{field}.{meth} 在 {typ} 上不存在")

if problems:
    print("发现问题:")
    for x in sorted(set(problems)):
        print("  -", x)
else:
    print("跨包引用检查通过：未发现不存在的符号")
