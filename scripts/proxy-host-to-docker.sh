#!/bin/bash
# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

BOLD='\033[1m'
NC='\033[0m'
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'

CONTAINER_NAME="scion-proxy-host"
IMAGE="alpine/socat"

usage() {
    echo "Usage: $0 [-l listen_addr] [-p port] [-t target_host:target_port] [-d] [-h]"
    echo ""
    echo "Forwards traffic from the Docker host network to a target host:port,"
    echo "allowing containers to reach services that are not directly accessible."
    echo ""
    echo "Arguments:"
    echo "  -t TARGET        Target host:port to forward to (required)"
    echo "  -l LISTEN        Listen address on Docker host (default: 0.0.0.0)"
    echo "  -p PORT          Port to listen on (default: 8091)"
    echo "  -d               Detached mode (run in background, default)"
    echo "  -i               Interactive mode (attach to container logs)"
    echo "  -s               Stop/remove existing proxy"
    echo "  -h               Show this help"
    echo ""
    echo "Examples:"
    echo "  $0 -t 192.168.4.31:8091"
    echo "  $0 -t 192.168.4.31:8091 -l 0.0.0.0 -p 8091"
    echo "  $0 -s"
    exit 0
}

TARGET=""
LISTEN="0.0.0.0"
PORT="8091"
MODE="detached"

while getopts "t:l:p:dsih" opt; do
    case $opt in
        t) TARGET="$OPTARG" ;;
        l) LISTEN="$OPTARG" ;;
        p) PORT="$OPTARG" ;;
        d) MODE="detached" ;;
        i) MODE="interactive" ;;
        s) MODE="stop" ;;
        h) usage ;;
        *) usage ;;
    esac
done

if [ "$MODE" = "stop" ]; then
    echo -e "${CYAN}[proxy]${NC} Stopping proxy container..."
    if docker ps -a --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
        docker stop "$CONTAINER_NAME" 2>/dev/null || true
        docker rm "$CONTAINER_NAME" 2>/dev/null || true
        echo -e "${GREEN}[proxy]${NC} Container ${BOLD}${CONTAINER_NAME}${NC} removed."
    else
        echo -e "${YELLOW}[proxy]${NC} No proxy container found."
    fi
    exit 0
fi

if [ -z "$TARGET" ]; then
    echo -e "${RED}[proxy]${NC} Error: Target host:port is required (-t)."
    echo ""
    usage
    exit 1
fi

# Validate target format
if ! echo "$TARGET" | grep -qE '^[0-9a-zA-Z._-]+:[0-9]+$'; then
    echo -e "${RED}[proxy]${NC} Error: Invalid target format '${TARGET}'. Expected host:port."
    exit 1
fi

TARGET_HOST="${TARGET%:*}"
TARGET_PORT="${TARGET##*:}"

check_tool() {
    if ! command -v "$1" &>/dev/null; then
        echo -e "${RED}[proxy]${NC} Error: '$1' is not installed."
        exit 1
    fi
}

check_tool docker

# Check if a proxy is already running
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo -e "${YELLOW}[proxy]${NC} A proxy is already running:"
    echo -e "  Container: ${BOLD}${CONTAINER_NAME}${NC}"
    echo -e "  Listening: ${BOLD}${LISTEN}:${PORT}${NC}"
    echo -e "  Target:    ${BOLD}${TARGET}${NC}"
    echo ""
    echo -e "  Stop it first with: $0 -s"
    exit 0
fi

echo -e "${CYAN}[proxy]${NC} Starting proxy container..."
echo -e "  Listen:  ${BOLD}${LISTEN}:${PORT}${NC}"
echo -e "  Target:  ${BOLD}${TARGET}${NC}"
echo ""

docker run --name "$CONTAINER_NAME" \
    --network host \
    -d \
    --restart unless-stopped \
    "$IMAGE" \
    "tcp-listen:${PORT},reuseaddr,fork" \
    "tcp-connect:${TARGET_HOST}:${TARGET_PORT}"

# Wait for container to start
sleep 1

if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER_NAME}$"; then
    echo -e "${GREEN}[proxy]${NC} Proxy is running. Containers can reach the target via:"
    echo ""
    
    # Determine the Docker bridge gateway
    DOCKER_GATEWAY=$(docker network inspect bridge -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}' 2>/dev/null || echo "172.17.0.1")
    
    echo -e "  ${BOLD}Docker Gateway IP:${NC} ${DOCKER_GATEWAY}"
    echo -e "  ${BOLD}Proxy Port:${NC}        ${PORT}"
    echo ""
    echo -e "  From inside a container, reach the target at:"
    echo -e "    ${GREEN}${DOCKER_GATEWAY}:${PORT}${NC}"
    echo ""
    echo -e "  Logs:  ${BOLD}docker logs -f ${CONTAINER_NAME}${NC}"
    echo -e "  Stop:  ${BOLD}$0 -s${NC}"
else
    echo -e "${RED}[proxy]${NC} Failed to start proxy container."
    echo -e "${YELLOW}[proxy]${NC} Container logs:"
    docker logs "$CONTAINER_NAME" 2>&1 || true
    docker rm "$CONTAINER_NAME" 2>/dev/null || true
    exit 1
fi
