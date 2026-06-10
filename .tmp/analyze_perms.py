import json
import glob
import os
import re
from collections import Counter

home = os.path.expanduser("~")
pattern = os.path.join(home, ".claude", "projects", "*", "*.jsonl")
files = glob.glob(pattern)
files.sort(key=os.path.getmtime, reverse=True)
files = files[:50]

counts = Counter()
mcp_counts = Counter()
examples = {}

def split_segments(cmd):
    # naive split on && and ; and | at top level (good enough for this analysis)
    parts = re.split(r'&&|\|\||;|\|', cmd)
    return [p.strip() for p in parts if p.strip()]

for f in files:
    try:
        with open(f, "r", encoding="utf-8", errors="ignore") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    obj = json.loads(line)
                except Exception:
                    continue
                msg = obj.get("message", {})
                content = msg.get("content")
                if not isinstance(content, list):
                    continue
                for item in content:
                    if not isinstance(item, dict):
                        continue
                    if item.get("type") != "tool_use":
                        continue
                    name = item.get("name", "")
                    inp = item.get("input", {})
                    if name == "Bash":
                        cmd = inp.get("command", "")
                        first_line = cmd.strip().split("\n")[0]
                        for seg in split_segments(first_line):
                            tokens = seg.split()
                            i = 0
                            while i < len(tokens) and (re.match(r'^[A-Za-z_][A-Za-z0-9_]*=', tokens[i]) or tokens[i] in ("sudo", "timeout")):
                                if tokens[i] == "timeout" and i+1 < len(tokens):
                                    i += 1
                                i += 1
                            tokens = tokens[i:]
                            if not tokens:
                                continue
                            base = tokens[0].strip('"\'')
                            sub = tokens[1].strip('"\'') if len(tokens) > 1 else ""
                            key = (base, sub)
                            counts[key] += 1
                            examples.setdefault(key, seg)
                    elif name.startswith("mcp__"):
                        mcp_counts[name] += 1
    except Exception:
        pass

with open(os.path.join(os.path.dirname(__file__), "perm_analysis.txt"), "w", encoding="utf-8") as out:
    out.write("=== Bash command+subcommand counts (top 60) ===\n")
    for key, c in counts.most_common(60):
        out.write(f"{c} {key} | {examples.get(key, '')}\n")
    out.write("\n=== MCP tool counts ===\n")
    for name, c in mcp_counts.most_common(20):
        out.write(f"{c} {name}\n")

print("done")
