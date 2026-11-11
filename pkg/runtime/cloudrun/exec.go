package cloudrun

import (
	"context"
	"io"
)

// ExecConnector abstracts how we exec into a Cloud Run Instance.
// The interface allows swapping the IAP implementation later
// (e.g., for official SDK support or gcloud delegation).
type ExecConnector interface {
	// Connect establishes an interactive shell session to the named instance.
	Connect(ctx context.Context, project, location, instanceName string) error
	// Exec runs a single command in the instance and returns output.
	Exec(ctx context.Context, project, location, instanceName string, cmd []string) ([]byte, error)
	// ExecWithStdin runs a single command in the instance with stdin piped
	// from the given reader, instead of embedding data in cmd. Used to
	// deliver secrets without exposing them in argv (see #1355).
	ExecWithStdin(ctx context.Context, project, location, instanceName string, cmd []string, stdin io.Reader) ([]byte, error)
}
