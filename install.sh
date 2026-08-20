#!/bin/bash

# Worker Transcode Installation Script
# Usage: curl -fsSL https://raw.githubusercontent.com/zergolf1994/worker-transcode/main/install.sh | sudo -E bash -s -- [OPTIONS]

set -e

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

# Defaults
WORKER_COUNT=1
UNINSTALL=false
DATABASE_URL=""
DASHBOARD_PORT="8886"
MEDIA_LAYOUT="muxed"

APP_NAME="worker-transcode"
APP_DIR="/opt/$APP_NAME"
SERVICE_NAME="worker-transcode"
GITHUB_REPO="zergolf1994/worker-transcode"
RELEASES_URL="https://github.com/$GITHUB_REPO/releases/latest/download"

print_status()  { echo -e "${GREEN}[INFO]${NC} $1"; }
print_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
print_error()   { echo -e "${RED}[ERROR]${NC} $1"; }

# Parse args
while [[ $# -gt 0 ]]; do
    case $1 in
        --uninstall)         UNINSTALL=true; shift ;;
        --count|-w|-n)       WORKER_COUNT="$2"; shift 2 ;;
        --database-url)      DATABASE_URL="$2"; shift 2 ;;
        --mongodb-uri)       DATABASE_URL="$2"; shift 2 ;; # alias เดิม
        --port)              DASHBOARD_PORT="$2"; shift 2 ;;
        --media-layout)      MEDIA_LAYOUT="$2"; shift 2 ;;
        -h|--help)
            echo "Worker Transcode Installer"
            echo ""
            echo "Usage: curl -fsSL https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh | sudo -E bash -s -- [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --uninstall          Uninstall completely"
            echo "  --count NUM          Number of worker instances (default: 1)"
            echo "  -w, -n NUM           Alias for --count"
            echo "  --database-url URI   MongoDB connection string (DATABASE_URL)"
            echo "  --port PORT          Realtime monitor port (default: 8886; worker @1 only)"
            echo "  --media-layout MODE  muxed (legacy) or separated (video-only + audio media)"
            echo "  -h, --help           Show this help"
            echo ""
            echo "Examples:"
            echo "  # Install with 1 worker"
            echo "  curl -fsSL https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh | sudo -E bash -s -- \\"
            echo "      --database-url \"mongodb+srv://user:pass@host/db\""
            echo ""
            echo "  # Install with 2 workers"
            echo "  curl -fsSL https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh | sudo -E bash -s -- \\"
            echo "      --database-url \"mongodb+srv://user:pass@host/db\" --count 2"
            echo ""
            echo "  # Uninstall"
            echo "  curl -fsSL https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh | sudo bash -s -- --uninstall"
            exit 0 ;;
        *)
            print_error "Unknown option: $1"; exit 1 ;;
    esac
done

if [ "$MEDIA_LAYOUT" != "muxed" ] && [ "$MEDIA_LAYOUT" != "separated" ]; then
    print_error "--media-layout must be muxed or separated"
    exit 1
fi

if [ "$UNINSTALL" = true ]; then
    print_warning "⚠️  Starting Uninstallation..."
    for i in $(seq 1 20); do
        systemctl stop "${SERVICE_NAME}@${i}"    2>/dev/null || true
        systemctl disable "${SERVICE_NAME}@${i}" 2>/dev/null || true
    done
    systemctl stop "${SERVICE_NAME}"    2>/dev/null || true
    systemctl disable "${SERVICE_NAME}" 2>/dev/null || true
    [ -f "/etc/systemd/system/${SERVICE_NAME}@.service" ] && rm "/etc/systemd/system/${SERVICE_NAME}@.service"
    [ -f "/etc/systemd/system/${SERVICE_NAME}.service"  ] && rm "/etc/systemd/system/${SERVICE_NAME}.service"
    systemctl daemon-reload
    [ -d "$APP_DIR" ] && rm -rf "$APP_DIR"
    print_status "✅ Uninstalled successfully!"
    exit 0
fi

# Check root
if [ "$(id -u)" -ne 0 ]; then
    print_error "This script must be run as root (use sudo)"
    exit 1
fi

print_status "🚀 Starting Installation... (Workers: $WORKER_COUNT)"

# transcode ใช้ ffmpeg/ffprobe encode — ต้องติดตั้งด้วย
# (GPU: ถ้ามี NVIDIA driver + nvenc ffmpeg จะ auto-detect เอง)
print_status "Installing system dependencies (curl, ffmpeg)..."
if command -v apt-get &>/dev/null; then
    apt-get update -qq
    apt-get install -y -qq curl ffmpeg
elif command -v yum &>/dev/null; then
    yum install -y curl ffmpeg
elif command -v dnf &>/dev/null; then
    dnf install -y curl ffmpeg
fi

for cmd in curl; do
    if ! command -v $cmd &>/dev/null; then
        print_error "$cmd not found. Please install it manually."
        exit 1
    fi
done

# The installer deliberately does not install/replace the kernel driver. If a
# working NVIDIA driver is already present, install the matching user-space
# encode/decode libraries that FFmpeg loads at runtime.
if command -v nvidia-smi &>/dev/null && nvidia-smi &>/dev/null; then
    NVIDIA_DRIVER_VERSION=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null | head -n 1 | tr -d '[:space:]')
    NVIDIA_DRIVER_BRANCH=${NVIDIA_DRIVER_VERSION%%.*}
    print_status "NVIDIA driver detected: $NVIDIA_DRIVER_VERSION (branch $NVIDIA_DRIVER_BRANCH)"

    if ! ldconfig -p 2>/dev/null | grep -q 'libnvidia-encode\.so\.1' || \
       ! ldconfig -p 2>/dev/null | grep -q 'libnvcuvid\.so\.1'; then
        if command -v apt-get &>/dev/null && apt-cache show "libnvidia-encode-$NVIDIA_DRIVER_BRANCH" &>/dev/null; then
            print_status "Installing NVIDIA codec runtime for branch $NVIDIA_DRIVER_BRANCH..."
            DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
                "libnvidia-encode-$NVIDIA_DRIVER_BRANCH" \
                "libnvidia-decode-$NVIDIA_DRIVER_BRANCH"
            ldconfig
        else
            print_warning "libnvidia-encode.so.1 is missing and no matching package was found."
            print_warning "Install libnvidia-encode-$NVIDIA_DRIVER_BRANCH and libnvidia-decode-$NVIDIA_DRIVER_BRANCH manually."
        fi
    fi

    if ffmpeg -hide_banner -loglevel error \
        -f lavfi -i color=c=black:s=256x256:d=1:r=25 \
        -pix_fmt yuv420p -c:v h264_nvenc -frames:v 1 -f null - &>/dev/null; then
        print_status "✅ NVIDIA NVENC runtime test passed (h264_nvenc)"
    else
        print_warning "NVIDIA driver is loaded, but the h264_nvenc runtime test failed."
        print_warning "Check: ldconfig -p | grep -E 'libnvidia-encode|libnvcuvid'"
    fi
else
    print_warning "NVIDIA driver not detected — worker will use CPU encoding until a driver is installed."
fi

print_status "Stopping existing services..."
systemctl stop ${SERVICE_NAME}@* 2>/dev/null || true
systemctl stop ${SERVICE_NAME}   2>/dev/null || true

print_status "Creating app directory: $APP_DIR"
mkdir -p "$APP_DIR"
cd "$APP_DIR"

ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    BINARY="linux"
elif [ "$ARCH" = "aarch64" ]; then
    BINARY="linux-arm64"
else
    print_error "Unsupported architecture: $ARCH"
    exit 1
fi

print_status "Downloading binary ($BINARY) from latest release..."
curl -fsSL "$RELEASES_URL/$BINARY" -o "$APP_DIR/$APP_NAME"
chmod +x "$APP_DIR/$APP_NAME"
print_status "Binary downloaded."

# ⚠ ตัวโปรแกรมอ่าน DATABASE_URL (ไม่ใช่ MONGODB_URI แบบระบบเก่า)
print_status "Creating .env file..."
cat > "$APP_DIR/.env" <<EOF
DATABASE_URL=$DATABASE_URL
DASHBOARD_PORT=$DASHBOARD_PORT
S3_UPLOAD_CONCURRENCY=2
MEDIA_LAYOUT=$MEDIA_LAYOUT
EOF

print_status "Creating systemd service template..."

cat > /etc/systemd/system/${SERVICE_NAME}@.service <<EOF
[Unit]
Description=Worker Transcode %i
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$APP_DIR
ExecStart=$APP_DIR/$APP_NAME
Restart=always
RestartSec=5
EnvironmentFile=$APP_DIR/.env
Environment="WORKER_ID=transcode_$(hostname)@%i"
# SIGTERM → worker คืนงานเข้าคิว (Release) + mark ตัวเอง offline ก่อนปิด
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
print_status "Starting $WORKER_COUNT worker(s)..."
for i in $(seq 1 $WORKER_COUNT); do
    systemctl enable ${SERVICE_NAME}@$i
    systemctl start  ${SERVICE_NAME}@$i
    sleep 0.3
done

sleep 2
RUNNING=0
for i in $(seq 1 $WORKER_COUNT); do
    systemctl is-active --quiet ${SERVICE_NAME}@$i && RUNNING=$((RUNNING+1))
done

echo ""
echo "============================================"
if [ $RUNNING -eq $WORKER_COUNT ]; then
    print_status "✅ Installation completed successfully!"
else
    print_warning "$RUNNING of $WORKER_COUNT workers running — check logs below"
    journalctl -u "${SERVICE_NAME}@1" -n 15 --no-pager
fi
echo "============================================"
echo ""
echo "  Directory:  $APP_DIR"
echo "  Workers:    $RUNNING / $WORKER_COUNT running"
echo "  Monitor:    http://$(hostname -I | awk '{print $1}'):$DASHBOARD_PORT (worker @1)"
echo ""
echo "  Commands:"
echo "    View logs:   journalctl -u \"${SERVICE_NAME}@*\" -f"
echo "    Worker 1:    journalctl -u \"${SERVICE_NAME}@1\" -f"
echo "    Restart all: for i in \$(seq 1 $WORKER_COUNT); do systemctl restart ${SERVICE_NAME}@\$i; done"
echo "    Stop all:    for i in \$(seq 1 $WORKER_COUNT); do systemctl stop ${SERVICE_NAME}@\$i; done"
echo "    Uninstall:   curl -fsSL https://raw.githubusercontent.com/$GITHUB_REPO/main/install.sh | sudo bash -s -- --uninstall"
echo "============================================"
