/*
Copyright 2025 The Scion Authors.
*/
package main

import (
	_ "time/tzdata" // embed the IANA tzdata database so time.LoadLocation works without /usr/share/zoneinfo

	"github.com/GoogleCloudPlatform/scion/cmd/sciontool/commands"
)

func main() {
	commands.Execute()
}
