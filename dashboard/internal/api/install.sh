#!/usr/bin/env sh
# UptimeMesh Agent 一键安装脚本(Linux + systemd)。
#
# 通常由 Dashboard「节点」页的「复制安装/卸载代码」生成,安装命令形如:
#   curl -fsSL http://<dashboard>/api/v1/agent/install.sh | sudo sh -s -- \
#     --server ws://<dashboard>/ws/agent --key <接入密钥> [--name <节点名>]
#
# 脚本做四件事:按架构下载 Agent 二进制、写入独立安装目录、
# 生成 systemd 单元(开机自启 + 崩溃重启)、启动服务。
set -eu

SERVER=""
KEY="${UM_ENROLLMENT_KEY:-}"
NAME=""
BIN_DIR="/usr/local/bin"
CONF_DIR="/etc/uptimemesh-agent"
STATE_DIR="/var/lib/uptimemesh-agent"
SERVICE="uptimemesh-agent"
UNIT_DIR="/etc/systemd/system"
UNIT="${UNIT_DIR}/${SERVICE}.service"
OVERWRITE="0"
NAME_SUFFIX=""
UNINSTALL="0"

usage() {
  cat >&2 <<'USAGE'
用法:
  install.sh --server <ws://host:port/ws/agent> [--key <接入密钥>] [--name <节点名>]
  install.sh --uninstall

  --server     Dashboard 的 Agent 接入地址,必填,ws:// 或 wss://
  --key        全局接入密钥;省略则在终端交互输入
  --name       节点名称;省略则取主机名
  --uninstall  交互选择卸载一个或全部本机 Agent
USAGE
  exit 2
}

while [ $# -gt 0 ]; do
  case "$1" in
    --server) SERVER="${2:-}"; shift 2 ;;
    --key)    KEY="${2:-}";    shift 2 ;;
    --name)   NAME="${2:-}";   shift 2 ;;
    --uninstall) UNINSTALL="1"; shift ;;
    -h|--help) usage ;;
    *) echo "未知参数: $1" >&2; usage ;;
  esac
done

[ "$(id -u)" -eq 0 ] || { echo "错误:需要 root 权限,请用 sudo 运行" >&2; exit 1; }
command -v systemctl >/dev/null 2>&1 || { echo "错误:未找到 systemd,本脚本仅支持 systemd 的 Linux" >&2; exit 1; }

instance_paths() {
  case "$1" in
    default)
      SERVICE="uptimemesh-agent"
      BIN="/usr/local/bin/uptimemesh-agent"
      CONF="/etc/uptimemesh-agent"
      STATE="/var/lib/uptimemesh-agent"
      ;;
    ''|*[!0-9]*|0)
      return 1
      ;;
    *)
      SERVICE="uptimemesh-agent-$1"
      BIN="/usr/local/lib/uptimemesh-agent/$1/uptimemesh-agent"
      CONF="/etc/uptimemesh-agent-$1"
      STATE="/var/lib/uptimemesh-agent-$1"
      ;;
  esac
  UNIT="/etc/systemd/system/${SERVICE}.service"
}

add_instance() {
  INSTANCE="$1"
  case "$INSTANCE" in
    ''|*[!0-9]*|0) return ;;
  esac
  instance_paths "$INSTANCE"
  if systemctl cat "${SERVICE}.service" >/dev/null 2>&1 || [ -e "$UNIT" ] || [ -e "$BIN" ] || [ -e "$CONF" ] || [ -e "$STATE" ]; then
    case " $FOUND " in
      *" $INSTANCE "*) ;;
      *) FOUND="${FOUND}${FOUND:+ }${INSTANCE}" ;;
    esac
  fi
}

uninstall_agents() {
  FOUND=""
  if systemctl cat uptimemesh-agent.service >/dev/null 2>&1 || [ -e /etc/systemd/system/uptimemesh-agent.service ] || [ -e /usr/local/bin/uptimemesh-agent ] || [ -e /etc/uptimemesh-agent ] || [ -e /var/lib/uptimemesh-agent ]; then
    FOUND="default"
  fi
  for UNIT_PATH in /etc/systemd/system/uptimemesh-agent-*.service; do
    [ -e "$UNIT_PATH" ] || continue
    INSTANCE="${UNIT_PATH##*/uptimemesh-agent-}"
    add_instance "${INSTANCE%.service}"
  done
  for INSTANCE_PATH in /usr/local/lib/uptimemesh-agent/*/uptimemesh-agent /etc/uptimemesh-agent-* /var/lib/uptimemesh-agent-*; do
    [ -e "$INSTANCE_PATH" ] || continue
    case "$INSTANCE_PATH" in
      /usr/local/lib/uptimemesh-agent/*/uptimemesh-agent)
        INSTANCE="${INSTANCE_PATH#/usr/local/lib/uptimemesh-agent/}"
        INSTANCE="${INSTANCE%/uptimemesh-agent}"
        ;;
      /etc/uptimemesh-agent-*) INSTANCE="${INSTANCE_PATH#/etc/uptimemesh-agent-}" ;;
      /var/lib/uptimemesh-agent-*) INSTANCE="${INSTANCE_PATH#/var/lib/uptimemesh-agent-}" ;;
    esac
    add_instance "$INSTANCE"
  done
  [ -n "$FOUND" ] || { echo "未发现已安装的 UptimeMesh Agent"; return 0; }

  echo "发现以下 Agent 实例:"
  for INSTANCE in $FOUND; do
    instance_paths "$INSTANCE"
    printf '  %s (%s)\n' "$INSTANCE" "$SERVICE"
  done
  printf '输入实例序号卸载单个,或输入 all 卸载全部: ' > /dev/tty 2>/dev/null || {
    echo "错误:卸载需要交互终端;未执行任何更改" >&2
    return 1
  }
  if ! IFS= read -r TARGET < /dev/tty; then
    echo "错误:无法读取卸载选择;未执行任何更改" >&2
    return 1
  fi
  case " $FOUND " in
    *" $TARGET "*) ;;
    *)
      if [ "$TARGET" != "all" ]; then
        echo "错误:选择无效;未执行任何更改" >&2
        return 1
      fi
      ;;
  esac
  printf '将删除所选 Agent 的服务、二进制、配置与凭据数据。请输入 yes 确认: ' > /dev/tty
  if ! IFS= read -r CONFIRM < /dev/tty || [ "$CONFIRM" != "yes" ]; then
    echo "已取消卸载,未执行任何更改"
    return 0
  fi

  for INSTANCE in $FOUND; do
    [ "$TARGET" = "all" ] || [ "$TARGET" = "$INSTANCE" ] || continue
    instance_paths "$INSTANCE"
    echo "==> 卸载 ${SERVICE}"
    systemctl disable --now "${SERVICE}.service" >/dev/null 2>&1 || true
    rm -f "$UNIT" "$BIN"
    rm -rf "$CONF" "$STATE"
  done
  systemctl daemon-reload
  echo "Agent 卸载完成"
}

if [ "$UNINSTALL" = "1" ]; then
  uninstall_agents
  exit $?
fi

[ -n "$SERVER" ] || { echo "错误:缺少 --server" >&2; usage; }

has_existing_install() {
  if systemctl cat "${SERVICE}.service" >/dev/null 2>&1 || [ -e "$UNIT" ] || [ -e "${BIN_DIR}/uptimemesh-agent" ] || [ -e "$CONF_DIR" ]; then
    return 0
  fi
  for EXISTING_UNIT in "${UNIT_DIR}/${SERVICE}"-*.service; do
    [ -e "$EXISTING_UNIT" ] && return 0
  done
  return 1
}

if has_existing_install; then
  printf '检测到已有 UptimeMesh Agent 安装(%s)。\n1) 覆盖安装并删除原有数据\n2) 同时存在,安装为独立实例\n请选择 [1/2]: ' "$SERVICE" > /dev/tty 2>/dev/null || {
    echo "错误:检测到已有安装,请在交互终端选择覆盖安装或同时存在" >&2
    exit 1
  }
  if ! IFS= read -r INSTALL_MODE < /dev/tty; then
    echo "错误:无法读取安装选择" >&2
    exit 1
  fi
  case "$INSTALL_MODE" in
    1)
      OVERWRITE="1"
      ;;
    2)
      INDEX=2
      while systemctl cat "${SERVICE}-${INDEX}.service" >/dev/null 2>&1 || [ -e "${UNIT_DIR}/${SERVICE}-${INDEX}.service" ] || [ -e "/etc/${SERVICE}-${INDEX}" ] || [ -e "/var/lib/${SERVICE}-${INDEX}" ] || [ -e "/usr/local/lib/uptimemesh-agent/${INDEX}/uptimemesh-agent" ]; do
        INDEX=$((INDEX + 1))
      done
      SERVICE="${SERVICE}-${INDEX}"
      UNIT="${UNIT_DIR}/${SERVICE}.service"
      BIN_DIR="/usr/local/lib/uptimemesh-agent/${INDEX}"
      CONF_DIR="/etc/${SERVICE}"
      STATE_DIR="/var/lib/${SERVICE}"
      NAME_SUFFIX="-${INDEX}"
      ;;
    *)
      echo "错误:请输入 1(覆盖安装) 或 2(同时存在)" >&2
      exit 2
      ;;
  esac
fi

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
NAME="${NAME}${NAME_SUFFIX:-}"

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

if [ "$OVERWRITE" = "1" ]; then
  echo "==> 停止并清理旧安装 ${SERVICE}"
  systemctl disable --now "${SERVICE}.service" >/dev/null 2>&1 || true
  rm -f "$UNIT" "${CONF_DIR}/env" "${BIN_DIR}/uptimemesh-agent"
  rm -rf "$CONF_DIR" "$STATE_DIR"
  systemctl daemon-reload
fi

mkdir -p "$BIN_DIR"
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
