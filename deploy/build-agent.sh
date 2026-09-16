#!/usr/bin/env bash
# 交叉编译 UptimeMesh Agent(linux/amd64、linux/arm64),输出到 bin/。
# 用法:  ./deploy/build-agent.sh          # 构建全部目标
#        ./deploy/build-agent.sh windows   # 追加 windows/amd64
#        AGENT_VERSION=0.2.0 ./deploy/build-agent.sh   # 显式指定版本号
set -euo pipefail
# 依赖下载走国内源,与 Dockerfile.dashboard / Dockerfile.agent 保持同一口径。
# 已显式设过 GOPROXY 的环境(CI、内网镜像)优先,不被脚本覆盖。
# 主源与兜底之间用 `|`(任意错误都回退),最后的 direct 用 `,`(仅 404/410 才直连)。
export GOPROXY="${GOPROXY:-https://goproxy.cn|https://mirrors.tencent.com/go,direct}"
cd "$(dirname "$0")/../agent"
mkdir -p ../bin
# 版本号:显式指定优先,否则用 git 描述(有 tag 用 tag,否则短哈希);两者都不行则 dev。
# 它会被打进二进制,同时写进 bin/VERSION——仪表盘据 bin/VERSION 判断节点是否需要
# 「一键升级」,因此两者必须完全一致,否则升级后会反复提示可升级。
VERSION="${AGENT_VERSION:-$(git -C .. describe --tags --always --dirty 2>/dev/null || echo dev)}"

build() {
  local goos=$1 goarch=$2
  local out=../bin/uptimemesh-agent-${goos}-${goarch}
  [ "$goos" = windows ] && out+=.exe
  echo "building $goos/$goarch -> $out"
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch \
    go build -trimpath \
    -ldflags "-s -w -X github.com/uptimemesh/shared/agentclient.Version=${VERSION}" \
    -o "$out" ./cmd
}

build linux amd64
build linux arm64
[ "${1:-}" = windows ] && build windows amd64
printf '%s\n' "$VERSION" > ../bin/VERSION
echo "done. version=$VERSION"
