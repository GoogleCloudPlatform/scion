package provision

import (
	"os"
	"testing"

	"github.com/GoogleCloudPlatform/scion/internal/testgit"
)

func TestMain(m *testing.M) {
	testgit.Setup()
	os.Exit(m.Run())
}
