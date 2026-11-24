#!/usr/bin/env bash
# RMTES 门禁变异自检：证明测试不是假绿。
# 两组变异：① 改错 C 表项；② 删除控制字符分支。
# 每组要求：变异后 go test 转红，还原后转绿。
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
cd "$ROOT" || exit 1

BAK=""
cleanup() {
  if [ -n "$BAK" ] && [ -d "$BAK" ]; then
    cp "$BAK/tables.go" "$ROOT/rmtes/tables.go"
    cp "$BAK/rmtes.go" "$ROOT/rmtes/rmtes.go"
    rm -rf "$BAK"
  fi
}
trap cleanup EXIT

run_tests() {
  env CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=mod go test ./rmtes >/dev/null 2>&1
}

fail=0
expect_red() {
  if run_tests; then
    echo "FAIL [$1] 变异后测试仍为绿（门禁假绿）"
    fail=1
  else
    echo "PASS [$1] 变异后测试转红"
  fi
}
expect_green() {
  if run_tests; then
    echo "PASS [$1] 还原后测试转绿"
  else
    echo "FAIL [$1] 还原后测试未转绿"
    fail=1
  fi
}

BAK="$(mktemp -d)"
cp rmtes/tables.go "$BAK/tables.go"
cp rmtes/rmtes.go "$BAK/rmtes.go"

echo "== 基线 =="
expect_green "基线"

echo "== 变异 1：C 表项改错（tblRBCS2[0] 0x00A1 -> 0x0041）=="
python3 - <<'PY'
import re
p = "rmtes/tables.go"
s = open(p, encoding="utf-8").read()
new, n = re.subn(r"(var tblRBCS2 = \[\.\.\.\]uint16\{\s*)0x00A1", r"\g<1>0x0041", s, count=1)
assert n == 1, "变异锚点未命中"
open(p, "w", encoding="utf-8", newline="\n").write(new)
PY
expect_red "表项改错"
cp "$BAK/tables.go" rmtes/tables.go
expect_green "表项还原"

echo "== 变异 2：删除控制字符分支（if in < 0x20 -> if in < 0x00）=="
python3 - <<'PY'
p = "rmtes/rmtes.go"
s = open(p, encoding="utf-8").read()
assert s.count("if in < 0x20 {") == 1, "变异锚点不唯一"
open(p, "w", encoding="utf-8", newline="\n").write(s.replace("if in < 0x20 {", "if in < 0x00 {"))
PY
expect_red "控制分支删除"
cp "$BAK/rmtes.go" rmtes/rmtes.go
expect_green "控制分支还原"

if [ "$fail" -ne 0 ]; then
  echo "MUTATION CHECK: FAIL"
  exit 1
fi
echo "MUTATION CHECK: PASS (2/2 变异均能转红)"
