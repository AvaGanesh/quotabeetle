#!/usr/bin/env bash
# Installs (if needed) and starts a single-replica TigerBeetle cluster for local development.
# Usage: scripts/dev-tigerbeetle.sh [--reset]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
DATA_DIR="$ROOT_DIR/.tigerbeetle"
BIN="$DATA_DIR/tigerbeetle"
DATA_FILE="$DATA_DIR/0_0.tigerbeetle"

TB_PORT="${TB_PORT:-3000}"
TB_CLUSTER_ID="${TB_CLUSTER_ID:-0}"

if [[ "${1:-}" == "--reset" ]]; then
  echo "Removing existing cluster data at $DATA_FILE"
  rm -f "$DATA_FILE"
fi

mkdir -p "$DATA_DIR"

install_binary() {
  if [[ -x "$BIN" ]]; then
    return
  fi

  local os url
  os="$(uname -s)"
  case "$os" in
    Darwin) url="https://mac.tigerbeetle.com" ;;
    Linux)  url="https://linux.tigerbeetle.com" ;;
    *)
      echo "Unsupported OS: $os. Install TigerBeetle manually: https://docs.tigerbeetle.com/quick-start/" >&2
      exit 1
      ;;
  esac

  echo "Downloading TigerBeetle into $DATA_DIR..."
  curl -Lo "$DATA_DIR/tigerbeetle.zip" "$url"
  unzip -o -d "$DATA_DIR" "$DATA_DIR/tigerbeetle.zip"
  rm -f "$DATA_DIR/tigerbeetle.zip"
  chmod +x "$BIN"
  "$BIN" version
}

format_cluster() {
  if [[ -f "$DATA_FILE" ]]; then
    echo "Data file already exists at $DATA_FILE, skipping format."
    return
  fi
  echo "Formatting single-replica cluster (cluster=$TB_CLUSTER_ID)..."
  "$BIN" format \
    --cluster="$TB_CLUSTER_ID" \
    --replica=0 \
    --replica-count=1 \
    --development \
    "$DATA_FILE"
}

install_binary
format_cluster

echo "Starting TigerBeetle on port $TB_PORT (cluster=$TB_CLUSTER_ID)..."
echo "Client address: 127.0.0.1:$TB_PORT"
exec "$BIN" start --addresses="$TB_PORT" --development "$DATA_FILE"
