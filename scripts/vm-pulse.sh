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


# --- Formatting Helpers ---
BOLD='\033[1m'
NC='\033[0m' # No Color
YELLOW='\033[1;33m'
CYAN='\033[0;36m'

print_header() {
    echo -e "\n${BOLD}${CYAN}=== $1 ===${NC}"
}

check_tool() {
    if ! command -v "$1" &> /dev/null; then
        echo -e "${YELLOW}[WARNING]${NC} Tool '$1' is not installed. Skipping this check."
        return 1
    fi
    return 0
}

# --- 1. Vital Signs: Host Memory & CPU Pressure ---
print_header "SYSTEM RESOURCE OVERVIEW"
check_tool "free" && free -h
echo ""
if [ -f /proc/pressure/cpu ]; then
    echo -e "${BOLD}CPU Pressure (PSI):${NC}"
    cat /proc/pressure/cpu
    echo -e "${BOLD}Memory Pressure (PSI):${NC}"
    cat /proc/pressure/memory
else
    echo "PSI metrics not available (check kernel version)."
fi

# --- 2. Neural Noise: Context Switching & Threads ---
print_header "SCHEDULER & THREAD DENSITY"
check_tool "vmstat" && vmstat 1 3

echo -e "\n${BOLD}Total Daemon Threads (dockerd/containerd):${NC}"
# shellcheck disable=SC2009 # counting threads (ps -eLo); pgrep cannot count threads
ps -eLo comm,pid | grep -cE 'dockerd|containerd'

echo -e "\n${BOLD}Top 10 Thread-Heavy Processes:${NC}"
ps -eo nlwp,pid,args --sort=-nlwp | head -n 11

# --- 3. The Docker Cortex: Container Performance ---
print_header "DOCKER TOP OFFENDERS"
if check_tool "docker"; then
    echo -e "${BOLD}Top 10 CPU Consumers:${NC}"
    docker stats --no-stream --format "table {{.CPUPerc}}\t{{.Name}}\t{{.MemUsage}}" | sed 's/%//g' | sort -rn | head -n 10
    
    echo -e "\n${BOLD}Checking for Cgroup Throttling (Top 5):${NC}"
    # This finds containers actually being restricted by the kernel
    find /sys/fs/cgroup/cpu/docker/ -name "cpu.stat" -exec grep -H "throttled_time" {} + 2>/dev/null | awk -F: '$2 > 0' | sort -nk2 -r | head -n 5
else
    echo "Docker not found or daemon unreachable."
fi

# --- 4. Deep Dive: I/O & Process Latency ---
print_header "I/O & LATENCY"
if check_tool "iostat"; then
    iostat -xz 1 2 | tail -n +3
fi

if check_tool "pidstat"; then
    echo -e "\n${BOLD}Top Context-Switching Tasks (Voluntary vs Involuntary):${NC}"
    pidstat -wt 1 1 | sort -rk12 | head -n 10
fi

echo -e "\n${CYAN}=== Diagnostic Complete ===${NC}\n"