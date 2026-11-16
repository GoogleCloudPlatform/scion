//go:build !unix

/*
Copyright 2026 The Scion Authors.
*/

package commands

const defaultPlaywrightFfmpegSeedDir = "/usr/local/share/scion-playwright-ffmpeg"

var (
	playwrightFfmpegSeedDir      = defaultPlaywrightFfmpegSeedDir
	runSeedPlaywrightFfmpegCache = seedPlaywrightFfmpegCache
)

// seedPlaywrightFfmpegCache is a no-op outside Unix container environments.
func seedPlaywrightFfmpegCache(_, _ string, _, _ int) {}
