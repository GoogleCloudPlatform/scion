package sub

import (
	"os"
	"testing"

	"example.com/fx/hubtest"
)

func TestMain(m *testing.M) { os.Exit(hubtest.RunTestMain(m)) }
