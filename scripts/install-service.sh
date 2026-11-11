#!/usr/bin/env bash
set -euo pipefail

# 检查 root 权限
if [[ $EUID -ne 0 ]]; then
   echo "错误：必须使用 root 权限执行此脚本 (例如 sudo bash $0)" >&2
   exit 1
fi

BIN_PATH="/opt/weiyun/weiyun"
CONFIG_PATH="/opt/weiyun/config.toml"
CACHE_DIR="/opt/weiyun/cache"
MOUNT_DIR="/mnt/weiyun"
SERVICE_FILE="/etc/systemd/system/weiyun.service"

echo "==> 检查前置文件和权限..."

if [[ ! -x "$BIN_PATH" ]]; then
    echo "错误：二进制文件 $BIN_PATH 不存在或不可执行！" >&2
    exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
    echo "错误：配置文件 $CONFIG_PATH 不存在！" >&2
    exit 1
fi

# 确保权限合规
chmod 755 "$BIN_PATH"
chmod 600 "$CONFIG_PATH"
mkdir -p "$CACHE_DIR"
chmod 700 "$CACHE_DIR"
fusermount3 -uz "$MOUNT_DIR" 2>/dev/null || umount -l "$MOUNT_DIR" 2>/dev/null || true
mkdir -p "$MOUNT_DIR"

# 检查 fuse3 / fusermount3
if ! command -v fusermount3 &>/dev/null && ! command -v fusermount &>/dev/null; then
    echo "==> 检测到缺少 FUSE 工具，尝试安装 fuse3..."
    if command -v apt-get &>/dev/null; then
        apt-get update -y && apt-get install -y fuse3
    else
        echo "警告：未找到 apt-get，请确保系统已安装 fuse3 或 fusermount3！" >&2
    fi
fi

echo "==> 生成 systemd 服务文件: $SERVICE_FILE"
cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Tencent Weiyun FUSE Mount
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStartPre=-/usr/bin/fusermount3 -uz ${MOUNT_DIR}
ExecStartPre=/bin/mkdir -p ${MOUNT_DIR}
ExecStart=${BIN_PATH} --config ${CONFIG_PATH} mount wy:/ ${MOUNT_DIR} --cache-dir ${CACHE_DIR}
ExecStopPost=-/usr/bin/fusermount3 -uz ${MOUNT_DIR}

KillSignal=SIGINT
TimeoutStopSec=15
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=multi-user.target
EOF

echo "==> 重新加载 systemd 并启动服务..."
systemctl daemon-reload
systemctl enable --now weiyun.service

echo ""
echo "=========================================="
if systemctl is-active --quiet weiyun.service; then
    echo "✅ 微云挂载服务已成功启动并设置开机自启！"
    echo "挂载目录: ${MOUNT_DIR}"
else
    echo "⚠️ 服务已配置并尝试启动，但当前状态为未激活，请检查日志:"
    echo "  journalctl -u weiyun.service -n 20 --no-pager"
fi
echo "=========================================="
echo "常用运维命令："
echo "  查看状态: systemctl status weiyun"
echo "  实时日志: journalctl -u weiyun -f"
echo "  停止挂载: systemctl stop weiyun"
echo "  重启挂载: systemctl restart weiyun"
