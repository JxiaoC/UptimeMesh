#!/bin/sh
# fetch-geoip.sh —— 在宿主上取得/更新节点地域库(GeoLite2-Country.mmdb)。
#
# 什么时候需要它:
#   镜像里已经内置了一份(构建时下载,见 deploy/Dockerfile.dashboard),正常部署
#   什么都不用做。但 GeoLite2 每周更新,新分配的公网 IP 段要过一阵才能在库里查到;
#   想不重建镜像就换库时,用这个脚本把新库下到 deploy/geoip/,再按
#   deploy/docker-compose.yml 里注释掉的那行把文件挂进容器即可。
#
# 用法:
#   sh deploy/fetch-geoip.sh                      # 下载官方镜像源(免注册)
#   sh deploy/fetch-geoip.sh <自己的 .mmdb 直链>   # 用官方 MaxMind 或内网镜像
#
# 说明:默认源是公开镜像 P3TERX/GeoLite.mmdb 的每日构建产物 —— 官方 MaxMind 的
# 下载地址需要注册账号换许可密钥,而这份数据本就需要定期更新,不值得把密钥固化到
# 部署脚本里。需要官方原始数据时,自行到 https://www.maxmind.com 下载 City/Country
# 的 .mmdb,再把它拷成 deploy/geoip/GeoLite2-Country.mmdb。
#
# 数据来源声明:本文件下载的数据由 MaxMind 制作(This product includes GeoLite2
# data created by MaxMind, available from https://www.maxmind.com)。
set -eu

url="${1:-https://github.com/P3TERX/GeoLite.mmdb/releases/latest/download/GeoLite2-Country.mmdb}"

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
out_dir="$script_dir/geoip"
out_file="$out_dir/GeoLite2-Country.mmdb"

# 与镜像里 GEOIP_DB_PATH 指向的文件同名同目录结构,挂载时不用额外配路径。
mkdir -p "$out_dir"

# 临时文件 + 原子替换:下载中途失败不会把上一份可用的库截断成半截(容器正在读它)。
tmp="$out_file.part"
trap 'rm -f "$tmp"' EXIT

echo "正在下载地域库: $url"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL --retry 3 --retry-delay 2 --retry-all-errors --max-time 600 -o "$tmp" "$url"
elif command -v wget >/dev/null 2>&1; then
  wget -q -O "$tmp" "$url"
else
  echo "错误:需要 curl 或 wget" >&2
  exit 1
fi

# 至少要有几 MB:避免把 GitHub/镜像的 HTML 错误页当成 .mmdb 存下来。
size=$(wc -c < "$tmp" | tr -d ' ')
if [ "$size" -lt 1000000 ]; then
  echo "错误:下载到的文件只有 ${size} 字节,不像是地域库(检查 URL 是否可用)" >&2
  exit 1
fi

mv "$tmp" "$out_file"
trap - EXIT

echo "已保存: $out_file (${size} 字节)"
echo
echo "在 deploy/docker-compose.yml 里取消下面这行的注释,即可让容器使用宿主上的这份库:"
echo "  - ./geoip/GeoLite2-Country.mmdb:/app/geoip/GeoLite2-Country.mmdb:ro"
echo "然后 docker compose -f deploy/docker-compose.yml up -d"
echo "启动日志会打印已加载的库路径与数据版本(已加载地域库: /app/geoip/… (数据版本 YYYY-MM-DD))。"
