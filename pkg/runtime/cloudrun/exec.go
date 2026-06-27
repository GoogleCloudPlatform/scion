package cloudrun

import "context"

// ExecConnector abstracts how we exec into a Cloud Run Instance.
// The interface allows swapping the IAP implementation later
// (e.g., for official SDK support or gcloud delegation).
type ExecConnector interface {
	// Connect establishes an interactive shell session to the named instance.
	Connect(ctx context.Context, project, location, instanceName string) error
	// Exec runs a single command in the instance and returns output.
	Exec(ctx context.Context, project, location, instanceName string, cmd []string) ([]byte, error)
}
