package hub

import "log/slog"

// Boot starts the moved service (staying code that logs is not reported).
func Boot() {
	slog.Info("boot")
	Start()
}
