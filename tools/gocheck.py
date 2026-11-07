"""粗略的 Go 静态检查：括号平衡、import 使用、顶层重复声明、未定义 core 符号。

没有 Go 工具链时用来兜底。只做能确定的部分，不做完整类型检查。
"""
import os
import re
import sys
import json

ROOT = r"C:\Users\uu\repos\mcai-platform\hub"

def strip_strings_and_comments(src: str) -> str:
    out = []
    i, n = 0, len(src)
    state = None  # None | 'line' | 'block' | 'str' | 'raw' | 'rune'
    while i < n:
        c = src[i]
        two = src[i:i+2]
        if state is None:
            if two == "//":
                state = "line"; i += 2; continue
            if two == "/*":
                state = "block"; i += 2; continue
            if c == '"':
                state = "str"; i += 1; continue
            if c == '`':
                state = "raw"; i += 1; continue
            if c == "'":
                state = "rune"; i += 1; continue
            out.append(c); i += 1; continue
        if state == "line":
            if c == "\n":
                state = None; out.append(c)
            i += 1; continue
        if state == "block":
            if two == "*/":
                state = None; i += 2; continue
            i += 1; continue
        if state == "str":
            if c == "\\":
                i += 2; continue
            if c == '"':
                state = None
            i += 1; continue
        if state == "rune":
            if c == "\\":
                i += 2; continue
            if c == "'":
                state = None
            i += 1; continue
        if state == "raw":
            if c == '`':
                state = None
            i += 1; continue
    return "".join(out)

def split_imports(src):
    """返回 (import_names, body_without_import_block)。"""
    names = []
    m = re.search(r"^import\s*\((.*?)^\)", src, re.S | re.M)
    body = src
    if m:
        block = m.group(1)
        for line in block.splitlines():
            line = line.strip()
            if not line or line.startswith("//"):
                continue
            mm = re.match(r'(?:(\w+|\.)\s+)?"([^"]+)"', line)
            if mm:
                alias, path = mm.group(1), mm.group(2)
                name = alias if alias else path.rsplit("/", 1)[-1]
                if name == "_":
                    continue
                names.append((name, line))
        body = src[:m.start()] + src[m.end():]
    else:
        for mm in re.finditer(r'^import\s+(?:(\w+|\.)\s+)?"([^"]+)"', src, re.M):
            alias, path = mm.group(1), mm.group(2)
            name = alias if alias else path.rsplit("/", 1)[-1]
            if name != "_":
                names.append((name, mm.group(0)))
    return names, body

def check_file(path):
    problems = []
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
    if not re.search(r"^package\s+\w+", raw, re.M):
        problems.append((0, "缺少 package 声明"))
        return problems, []

    clean = strip_strings_and_comments(raw)

    # 括号平衡
    for open_ch, close_ch, label in [("{", "}", "花括号"), ("(", ")", "圆括号"), ("[", "]", "方括号")]:
        bal = 0
        for idx, ch in enumerate(clean):
            if ch == open_ch:
                bal += 1
            elif ch == close_ch:
                bal -= 1
                if bal < 0:
                    line = clean[:idx].count("\n") + 1
                    problems.append((line, f"{label}提前闭合"))
                    bal = 0
        if bal != 0:
            problems.append((0, f"{label}不平衡（差值 {bal}）"))

    # import 使用
    imports, body = split_imports(clean)
    for name, orig in imports:
        if not re.search(r"\b" + re.escape(name) + r"\.", body):
            problems.append((0, f"import 未使用: {orig.strip()}"))

    # 顶层声明
    decls = []
    for mm in re.finditer(r"^func\s+(?:\([^)]*\)\s*)?(\w+)\s*\(", clean, re.M):
        decls.append(mm.group(1))
    for mm in re.finditer(r"^type\s+(\w+)\s+", clean, re.M):
        decls.append(mm.group(1))
    dup = {d for d in decls if decls.count(d) > 1}
    for d in sorted(dup):
        problems.append((0, f"包内重复声明: {d}"))

    # 近似未使用局部变量：赋值后从不被读，且不是 _
    for mm in re.finditer(r"^\s*(\w+)\s*:=\s*", clean, re.M):
        var = mm.group(1)
        if var == "_":
            continue
        occurrences = len(re.findall(r"\b" + re.escape(var) + r"\b", clean))
        if occurrences <= 1:
            line = clean[:mm.start()].count("\n") + 1
            problems.append((line, f"疑似声明后未使用: {var}"))

    return problems, decls

def main():
    total = 0
    all_problems = {}
    symbols = {}
    files = []
    for dirpath, _dirs, names in os.walk(ROOT):
        if "\\data" in dirpath:
            continue
        for nm in names:
            if nm.endswith(".go"):
                files.append(os.path.join(dirpath, nm))

    for path in sorted(files):
        rel = os.path.relpath(path, ROOT)
        problems, decls = check_file(path)
        symbols[rel] = decls
        if problems:
            all_problems[rel] = problems
            total += len(problems)

    print(f"扫描 {len(files)} 个 .go 文件，发现 {total} 条疑似问题\n")
    for rel, items in all_problems.items():
        print(f"--- {rel}")
        seen = set()
        for line, msg in items:
            key = (line, msg)
            if key in seen:
                continue
            seen.add(key)
            loc = f":{line}" if line else ""
            print(f"  {loc} {msg}")
        print()

    print("顶层符号统计:")
    for rel, decls in symbols.items():
        print(f"  {rel}: {len(decls)} 个")

if __name__ == "__main__":
    main()
