"""查找包内“裸函数调用”但包内未定义、也不是内置/import 的情况。"""
import os
import re
import sys

ROOT = r"C:\Users\uu\repos\mcai-platform\hub"

BUILTINS = {
    "make", "len", "cap", "append", "copy", "delete", "new", "panic", "recover",
    "print", "println", "close", "complex", "real", "imag", "min", "max", "clear",
    "string", "int", "int8", "int16", "int32", "int64", "uint", "uint8", "uint16",
    "uint32", "uint64", "uintptr", "float32", "float64", "byte", "rune", "bool",
    "any", "error", "if", "for", "switch", "case", "return", "func", "go", "defer",
    "range", "select", "map", "chan", "struct", "interface", "type", "var", "const",
    "package", "import", "else", "break", "continue", "fallthrough", "default", "goto",
}

def strip(src):
    src = re.sub(r"/\*.*?\*/", "", src, flags=re.S)
    src = re.sub(r"//[^\n]*", "", src)
    out, i, n, state = [], 0, len(src), None
    while i < n:
        c = src[i]
        if state is None:
            if c == '"':
                state = "s"; i += 1; continue
            if c == '`':
                state = "r"; i += 1; continue
            if c == "'":
                state = "c"; i += 1; continue
            out.append(c); i += 1; continue
        if state == "s":
            if c == "\\":
                i += 2; continue
            if c == '"':
                state = None
            i += 1; continue
        if state == "c":
            if c == "\\":
                i += 2; continue
            if c == "'":
                state = None
            i += 1; continue
        if state == "r":
            if c == '`':
                state = None
            i += 1; continue
    return "".join(out)

def go_files():
    out = []
    for dirpath, _d, names in os.walk(ROOT):
        if os.sep + "data" in dirpath:
            continue
        for n in names:
            if n.endswith(".go"):
                out.append(os.path.join(dirpath, n))
    return out

# 按目录（包）分组
pkg_defs = {}
pkg_files = {}
for p in go_files():
    d = os.path.dirname(p)
    pkg_files.setdefault(d, []).append(p)
    src = strip(open(p, encoding="utf-8").read())
    defs = pkg_defs.setdefault(d, set())
    for m in re.finditer(r"^func\s+(?:\([^)]*\)\s*)?(\w+)\s*\(", src, re.M):
        defs.add(m.group(1))
    for m in re.finditer(r"^type\s+(\w+)\b", src, re.M):
        defs.add(m.group(1))
    for m in re.finditer(r"^var\s+(\w+)\b", src, re.M):
        defs.add(m.group(1))
    for m in re.finditer(r"^const\s+(\w+)\b", src, re.M):
        defs.add(m.group(1))
    for blk in re.finditer(r"^var\s*\((.*?)^\)", src, re.S | re.M):
        for line in blk.group(1).splitlines():
            mm = re.match(r"\s*(\w+)\s", line)
            if mm:
                defs.add(mm.group(1))

problems = []
for d, files in pkg_files.items():
    defs = pkg_defs[d]
    for p in files:
        src = strip(open(p, encoding="utf-8").read())
        # 局部变量与参数名（粗粒度）
        locals_ = set(re.findall(r"\b(\w+)\s*:=", src))
        locals_ |= set(re.findall(r"\b(\w+)\s+\*?\w[\w\.\[\]]*\s*[,)]", src))
        locals_ |= set(re.findall(r"for\s+(\w+)\s*,?\s*(\w+)?\s*:=", src) and [])
        for m in re.finditer(r"(?<![\w\.\)\]])([a-zA-Z_]\w*)\(", src):
            name = m.group(1)
            if name in BUILTINS or name in defs or name in locals_:
                continue
            if name in {"func", "if", "for", "switch", "return"}:
                continue
            line = src[:m.start()].count("\n") + 1
            problems.append(f"{os.path.relpath(p, ROOT)}:{line} 未定义调用 {name}(...)")

for x in sorted(set(problems)):
    print(x)
print(f"\n共 {len(set(problems))} 条（含局部变量识别的误报，需要人工确认）")
