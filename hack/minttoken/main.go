// Command minttoken mints a user access-token JWT for API-level integration
// testing against the running hubs. It looks up an existing (preferably admin)
// user in the shared Postgres DB and signs a token with the per-hub signing key
// read from Secret Manager. Not part of the product; used only for test driving.
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/GoogleCloudPlatform/scion/pkg/hub"
)

func main() {
	dsn := os.Getenv("PG_DSN")
	keyB64 := os.Getenv("SIGNING_KEY_B64")
	if dsn == "" || keyB64 == "" {
		fmt.Fprintln(os.Stderr, "PG_DSN and SIGNING_KEY_B64 required")
		os.Exit(1)
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decode key:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db connect:", err)
		os.Exit(1)
	}
	defer conn.Close(ctx)

	var id, email, displayName, role string
	// Prefer an admin; fall back to any user.
	err = conn.QueryRow(ctx, `SELECT id::text, email, display_name, role FROM users
		ORDER BY (role = 'admin') DESC, created ASC LIMIT 1`).Scan(&id, &email, &displayName, &role)
	if err != nil {
		fmt.Fprintln(os.Stderr, "user lookup:", err)
		os.Exit(1)
	}

	svc, err := hub.NewUserTokenService(hub.UserTokenConfig{SigningKey: key})
	if err != nil {
		fmt.Fprintln(os.Stderr, "token service:", err)
		os.Exit(1)
	}
	// CLI client type → long (30-day) validity so the token outlives the test run.
	token, _, err := svc.GenerateAccessToken(id, email, displayName, role, hub.ClientTypeCLI)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint:", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "user=%s email=%s role=%s\n", id, email, role)
	fmt.Println(token)
}
