#!/usr/bin/env sh
# 让 Agent 修订号 +1(约定见根目录 AGENTS.md 的「Agent 版本号」一节)。
#
# 背景:AGENT_VERSION 会被 -ldflags 打进 Agent 二进制,同时写进分发目录的 VERSION,
# 节点页据此判断「可升级 / 需重装」。改了 agent/ 或 shared/ 却没 +1,新旧二进制就会
# 报同一个版本号,节点页看不出落后 —— 线上踩过:重定向跟随修好后,旧节点仍把 302
# 判成失败,而页面没有任何提示。
#
# 用法:
#   sh deploy/bump-agent-version.sh          # 修订号 +1(0.1.0 -> 0.1.1)
#   sh deploy/bump-agent-version.sh 0.2.0    # 显式指定新版本号
set -eu

cd "$(dirname "$0")"
FILE=docker-compose.yml
[ -f "$FILE" ] || { echo "错误:找不到 $FILE" >&2; exit 1; }

# 取默认值(compose 里形如 AGENT_VERSION: ${AGENT_VERSION:-0.1.0})。
CUR="$(sed -n 's/.*AGENT_VERSION: [\$][{]AGENT_VERSION:-\([0-9][0-9]*[.][0-9][0-9]*[.][0-9][0-9]*\)[}].*/\1/p' "$FILE" | head -n 1)"
[ -n "$CUR" ] || {
  echo "错误:未在 $FILE 里找到形如 \${AGENT_VERSION:-x.y.z} 的默认版本号" >&2
  exit 1
}

if [ "$#" -ge 1 ]; then
  NEW="$1"
else
  BASE="${CUR%.*}"
  REV="${CUR##*.}"
  NEW="${BASE}.$((REV + 1))"
fi
case "$NEW" in
  [0-9]*[.][0-9]*[.][0-9]*) ;;
  *) echo "错误:版本号须形如 x.y.z,收到 $NEW" >&2; exit 1 ;;
esac

# 点号按字面匹配(它在正则里是通配符)。
CUR_RE="$(printf '%s' "$CUR" | sed 's/[.]/[.]/g')"
PAT="AGENT_VERSION: [\$][{]AGENT_VERSION:-${CUR_RE}[}]"
REP="AGENT_VERSION: \${AGENT_VERSION:-${NEW}}"

sed -i.bak "s/${PAT}/${REP}/g" "$FILE"
rm -f "$FILE.bak"
CHANGED="$(grep -c "AGENT_VERSION:-${NEW}}" "$FILE" || true)"
[ "$CHANGED" -gt 0 ] || { echo "错误:替换失败,$FILE 未改动" >&2; exit 1; }
echo "deploy/docker-compose.yml: AGENT_VERSION ${CUR} -> ${NEW}(改了 ${CHANGED} 处)"

# 源码默认值(没打 -ldflags 时 Agent 自报的版本,也是分发目录缺 VERSION 时仪表盘的回退值)
# 必须与发布版本一致:否则本地 go build/go run 出来的 Agent 会一直显示「可升级」。
CLIENT=../shared/agentclient/client.go
if [ -f "$CLIENT" ]; then
  SRC="$(sed -n 's/^var Version = "\([0-9][0-9]*[.][0-9][0-9]*[.][0-9][0-9]*\)".*/\1/p' "$CLIENT" | head -n 1)"
  if [ "$SRC" = "$CUR" ]; then
    sed -i.bak "s/^var Version = \"${CUR_RE}\"/var Version = \"${NEW}\"/" "$CLIENT"
    rm -f "$CLIENT.bak"
    echo "shared/agentclient/client.go: Version ${CUR} -> ${NEW}"
  elif [ "$SRC" != "$NEW" ]; then
    echo "提醒:$CLIENT 的源码默认值是 ${SRC}(既不是 ${CUR} 也不是 ${NEW}),请手工对齐。" >&2
  fi
fi

if [ -f .env ] && grep -q '^[[:space:]]*AGENT_VERSION=' .env 2>/dev/null; then
  echo "提醒:deploy/.env 里也设了 AGENT_VERSION,它会覆盖上面的默认值,请一并对齐。" >&2
fi
echo "接着重建并重启仪表盘(节点页据此显示可升级/需重装):"
echo "  docker compose -f deploy/docker-compose.yml build dashboard && docker compose -f deploy/docker-compose.yml up -d"
