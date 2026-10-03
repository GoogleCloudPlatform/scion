package main

import (
	"os"
	"strconv"
	"strings"
)

// readLoadAvg reads /proc/loadavg (Linux only). Returns zeros on any
// error (e.g. running on macOS during local development), which is
// visibly distinguishable from a genuine zero load average by checking
// MachineInfo.GOOS in the report.
//
// Recorded automatically because budgets cannot be chosen from this host --
// a shared, variably-loaded container -- and a future reader needs the
// load figure alongside the latency numbers to tell noise from signal.
func readLoadAvg() (load1, load5, load15 float64) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	load1, _ = strconv.ParseFloat(fields[0], 64)
	load5, _ = strconv.ParseFloat(fields[1], 64)
	load15, _ = strconv.ParseFloat(fields[2], 64)
	return load1, load5, load15
}

// readUptimeSeconds reads /proc/uptime (Linux only). Returns 0 on any error.
func readUptimeSeconds() float64 {
	data, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 1 {
		return 0
	}
	uptime, _ := strconv.ParseFloat(fields[0], 64)
	return uptime
}
