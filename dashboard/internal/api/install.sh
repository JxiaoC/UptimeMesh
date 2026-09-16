#!/usr/bin/env sh
# UptimeMesh Agent 一键安装脚本(Linux + systemd)。
#
# 通常由 Dashboard「节点」页的「复制安装代码」生成,形如:
#   curl -fsSL http://<dashboard>/api/v1/agent/install.sh | sudo sh -s -- \
#     --server ws://<dashboard>/ws/agent --key <接入密钥> [--name <节点名>]
#
# 脚本做四件事:按架构下载 Agent 二进制、写入 /usr/local/bin、
# 生成 systemd 单元(开机自启 + 崩溃重启)、启动服务。
set -eu

SERVER=""
KEY="${UM_ENROLLMENT_KEY:-}"
NAME=""
BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/uptimemesh-agent"
STATE_DIR="/var/lib/uptimemesh-agent"
SERVICE="uptimemesh-agent"
UNIT="/etc/systemd/system/${SERVICE}.service"

usage() {
  cat >&2 <<'USAGE'
用法: install.sh --server <ws://host:port/ws/agent> [--key <接入密钥>] [--name <节点名>]

  --server  Dashboard 的 Agent 接入地址,必填,ws:// 或 wss://
  --key     全局接入密钥;省略则在终端交互输入
  --name    节点名称;省略则取主机名
USAGE
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --key)    KEY="${2:-}";    shift 2 ;;
    --name)   NAME="${2:-}";   shift 2 ;;
    -h|--help) usage ;;
    *) echo "未知参数: $1" >&2; usage ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "错误:需要 root 权限,请用 sudo 运行" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "错误:未找到 systemd,本脚本仅支持 systemd 的 Linux" >&2; exit 1; }
[ -n "$SERVER" ] || { echo "错误:缺少 --server" >&2; usage; }

case "$SERVER" in
  ws://*|wss://*) ;;
  *) echo "错误:--server 必须以 ws:// 或 wss:// 开头" >&2; exit 2 ;;
esac

# 从接入地址推导安装包下载地址(wss->https、ws->http,丢弃 /ws/agent 路径)。
SCHEME="${SERVER%%://*}"
REST="${SERVER#*://}"
HOSTPORT="${REST%%/*}"
if [ "$SCHEME" = "wss" ]; then HTTP_SCHEME="https"; else HTTP_SCHEME="http"; fi
BASE="${HTTP_SCHEME}://${HOSTPORT}"

# 密钥缺失时交互补全(不落 shell 历史)。
if [ -z "$KEY" ]; then
  if [ -r /dev/tty ]; then
    printf '请输入接入密钥(输入不回显): ' > /dev/tty
    read -r KEY < /dev/tty || true
    printf '\n' > /dev/tty
  fi
fi
[ -n "$KEY" ] || { echo "错误:缺少接入密钥(用 --key 传入或提供终端交互)" >&2; exit 2; }

# 架构探测,须与 deploy/build-agent.sh 的产出对应。
MACHINE="$(uname -m)"
case "$MACHINE" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "错误:暂不支持架构 $MACHINE(仅 linux/amd64、linux/arm64)" >&2; exit 1 ;;
esac

[ -n "$NAME" ] || NAME="$(hostname 2>/dev/null || echo agent)"

DOWNLOAD_URL="${BASE}/api/v1/agent/download/linux-${ARCH}"
echo "==> 下载 Agent (linux/${ARCH}): ${DOWNLOAD_URL}"
TMP="$(mktemp)"
trap 'rm -f "$TMP"' EXIT INT TERM
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$DOWNLOAD_URL" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$TMP" "$DOWNLOAD_URL"
else
  echo "错误:需要 curl 或 wget" >&2; exit 1
fi
install -m 0755 "$TMP" "${BIN_DIR}/uptimemesh-agent"

echo "==> 写入配置 ${CONF_DIR}/env"
mkdir -p "$CONF_DIR" "$STATE_DIR"
# 值加引号,避免名称/路径含空格时被 systemd 环境文件截断。
cat > "${CONF_DIR}/env" <<EOF
UM_SERVER="${SERVER}"
UM_KEY="${KEY}"
UM_NAME="${NAME}"
UM_CRED_DIR="${STATE_DIR}"
EOF
chmod 600 "${CONF_DIR}/env"

echo "==> 写入 systemd 单元 ${UNIT}"
mkdir -p "$(dirname "$UNIT")"
cat > "$UNIT" <<EOF
[Unit]
Description=UptimeMesh Agent
Documentation=${BASE}
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${CONF_DIR}/env
ExecStart=${BIN_DIR}/uptimemesh-agent --server \${UM_SERVER} --key \${UM_KEY} --name \${UM_NAME} --cred-dir \${UM_CRED_DIR}
Restart=always
RestartSec=5
# PING(ICMP)需要原始套接字能力
AmbientCapabilities=CAP_NET_RAW
CapabilityBoundingSet=CAP_NET_RAW

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable "${SERVICE}" >/dev/null 2>&1
systemctl restart "${SERVICE}"
sleep 1
systemctl --no-pager --lines=0 status "${SERVICE}" || true

cat <<EOF

==> 安装完成。Agent 已启动并设为开机自启。
    - 服务名:${SERVICE}    查看日志:journalctl -u ${SERVICE} -f
    - 接入密钥已保存在 ${CONF_DIR}/env(仅 root 可读);批准后 Agent 改用
      ${STATE_DIR}/credential,届时可轮换全局密钥不影响本节点。
    - 若节点仍显示「待审批」:打开 ${BASE} → 节点页 → 对该节点点「批准」。
EOF
