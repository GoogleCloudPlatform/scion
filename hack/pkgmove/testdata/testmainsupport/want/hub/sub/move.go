package sub

import "os"

func Home() string { return os.Getenv("HOME") }
