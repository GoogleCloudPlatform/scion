package cloudrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"
)

type IAPExecConnector struct {
	IapTunnelUrlOverride  string
	IapTunnelInsecureTLS  bool
	SSHUser               string
	SSHHost               string
	StrictHostKeyChecking string
	UserKnownHostsFile    string
	SSHDebug              bool

	// newInstancesClient overrides how the Instances API client used to look
	// up the instance's service account is constructed. Nil in production
	// (NewInstancesClient); tests inject a fake.
	newInstancesClient func(ctx context.Context) (InstancesAPI, error)
	// detectCredentials overrides how the token source is built. Nil in
	// production (Application Default Credentials); tests inject a fake.
	detectCredentials func() (tokenSource, error)
	// lookupProjectNumber overrides how a project ID is resolved to its
	// number. Nil in production (fetchProjectNumber); tests inject a fake.
	lookupProjectNumber func(ctx context.Context, projectID string) (string, error)
	// now overrides the clock used for signed key expiry. Nil in production
	// (time.Now).
	now func() time.Time
	// sshRunner overrides one ssh run. Nil in production (runSSH); tests
	// inject a fake to observe the authentication retry.
	sshRunner func(ctx context.Context, project, location, instanceName string, cmdArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error

	credsMu sync.Mutex
	creds   tokenSource

	// instancesClient is created on first use and kept for the connector's
	// lifetime; see Close.
	clientMu        sync.Mutex
	instancesClient InstancesAPI

	mu             sync.Mutex
	projectNumbers map[string]string
	signedKeys     map[string]*signedKey
}

// tokenSource yields OAuth2 access tokens. *auth.Credentials implements it
// and caches and refreshes its token internally.
type tokenSource interface {
	Token(ctx context.Context) (*auth.Token, error)
}

// signedKey is an SSH key pair and its OS Login certificate for one instance.
type signedKey struct {
	instanceUID    string
	serviceAccount string
	privKeyPEM     []byte
	cert           string
	reuseUntil     time.Time
}

const (
	// signedKeyMinRemaining is how much validity a cached certificate must
	// have left to be reused.
	signedKeyMinRemaining = 60 * time.Second
	// signedKeyDefaultValidity is assumed when the certificate's expiry
	// cannot be read.
	signedKeyDefaultValidity = 4 * time.Minute
)

// errSSHAuthFailed marks an ssh run rejected during public key
// authentication.
var errSSHAuthFailed = errors.New("SSH authentication failed")

// osLoginBaseURL is the OS Login API endpoint. Tests point it at a local
// server.
var osLoginBaseURL = "https://oslogin.googleapis.com"

func NewIAPExecConnector(iapTunnelUrlOverride string) *IAPExecConnector {
	return &IAPExecConnector{
		IapTunnelUrlOverride: iapTunnelUrlOverride,
	}
}

func (c *IAPExecConnector) Connect(ctx context.Context, project, location, instanceName string) error {
	return c.runWithAuthRetry(project, location, instanceName, true, func() error {
		return c.runOnce(ctx, project, location, instanceName, nil, os.Stdin, os.Stdout, os.Stderr)
	})
}

func (c *IAPExecConnector) Exec(ctx context.Context, project, location, instanceName string, cmd []string) ([]byte, error) {
	var outBuf bytes.Buffer
	err := c.runWithAuthRetry(project, location, instanceName, true, func() error {
		outBuf.Reset()
		return c.runOnce(ctx, project, location, instanceName, cmd, nil, &outBuf, &outBuf)
	})
	return outBuf.Bytes(), err
}

// ExecWithStdin runs cmd over the same IAP SSH tunnel as Exec, but attaches
// stdin so callers can deliver secrets without putting them in cmd (which
// becomes part of the remote command line). See #1355.
func (c *IAPExecConnector) ExecWithStdin(ctx context.Context, project, location, instanceName string, cmd []string, stdin io.Reader) ([]byte, error) {
	// The stdin reader cannot be replayed, so an authentication failure drops
	// the cached key without retrying.
	var outBuf bytes.Buffer
	err := c.runWithAuthRetry(project, location, instanceName, false, func() error {
		return c.runOnce(ctx, project, location, instanceName, cmd, stdin, &outBuf, &outBuf)
	})
	return outBuf.Bytes(), err
}

// runOnce runs one ssh session through sshRunner, or runSSH by default.
func (c *IAPExecConnector) runOnce(ctx context.Context, project, location, instanceName string, cmdArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if c.sshRunner != nil {
		return c.sshRunner(ctx, project, location, instanceName, cmdArgs, stdin, stdout, stderr)
	}
	return c.runSSH(ctx, project, location, instanceName, cmdArgs, stdin, stdout, stderr)
}

// runWithAuthRetry runs attempt. When it fails SSH authentication, the
// instance's cached signed key is dropped and, if retry is set, attempt runs
// once more with a freshly signed key.
//
// Authentication failure is recognised from ssh's exit status 255 and
// sshAuthFailureMarker on stderr, which the remote command shares. A remote
// command that prints the marker and exits 255 therefore makes Connect or
// Exec run it once more; ExecWithStdin never re-runs. Exec callers whose
// commands are not idempotent: tmux key and paste delivery to an agent
// (pkg/agent manager message delivery), the broker's exec endpoint, which
// runs a caller-supplied command, and the broker's "kill -USR2 1" reload
// signal (pkg/runtimebroker handlers).
func (c *IAPExecConnector) runWithAuthRetry(project, location, instanceName string, retry bool, attempt func() error) error {
	err := attempt()
	if !errors.Is(err, errSSHAuthFailed) {
		return err
	}
	c.Forget(project, location, instanceName)
	if !retry {
		return err
	}
	return attempt()
}

// Forget drops the cached signed key for the named instance.
func (c *IAPExecConnector) Forget(project, location, instanceName string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.signedKeys, instanceResourceName(project, location, instanceShortID(instanceName)))
}

func (c *IAPExecConnector) runSSH(ctx context.Context, project, location, instanceName string, cmdArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	authCreds, err := c.tokenSource()
	if err != nil {
		return fmt.Errorf("failed to detect credentials: %v", err)
	}
	tok, err := authCreds.Token(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve auth token: %v", err)
	}

	projectNumber, err := c.projectNumber(ctx, project)
	if err != nil {
		return fmt.Errorf("failed to resolve project number: %v", err)
	}

	shortID := instanceShortID(instanceName)

	key, err := c.signedKeyForInstance(ctx, project, location, shortID, tok.Value)
	if err != nil {
		return err
	}

	tempDir, err := os.MkdirTemp("", "run-ssh-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	privKeyPath := filepath.Join(tempDir, "id_rsa")
	err = os.WriteFile(privKeyPath, key.privKeyPEM, 0600)
	if err != nil {
		return fmt.Errorf("failed to write private key: %v", err)
	}

	certPath := filepath.Join(tempDir, "id_rsa-cert.pub")
	err = os.WriteFile(certPath, []byte(key.cert), 0644)
	if err != nil {
		return fmt.Errorf("failed to write SSH certificate: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("failed to start local TCP proxy: %v", err)
	}
	defer func() { _ = listener.Close() }()
	localPort := strings.Split(listener.Addr().String(), ":")[1]

	baseURL := "wss://tunnel.cloudproxy.app/v4"
	if c.IapTunnelUrlOverride != "" {
		baseURL = c.IapTunnelUrlOverride
	}
	wsURL := fmt.Sprintf("%s/connect?project=%s&port=22&newWebsocket=True&project_number=%s&cr_workload_type=instance&cr_deployment_name=%s&region=%s",
		baseURL, project, projectNumber, shortID, location)

	headers := http.Header{}
	headers.Add("Authorization", "Bearer "+tok.Value)
	headers.Add("Origin", "bot:iap-tunneler")

	var tlsConfig *tls.Config
	if c.IapTunnelUrlOverride != "" {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: c.IapTunnelInsecureTLS,
		}
		if u, err := url.Parse(c.IapTunnelUrlOverride); err == nil {
			tlsConfig.ServerName = u.Hostname()
		}
	}

	dialer := websocket.Dialer{
		Subprotocols:     []string{"relay.tunnel.cloudproxy.app"},
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  tlsConfig,
	}

	wsConn, resp, err := dialer.Dial(wsURL, headers)
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("failed to connect to IAP WebSocket tunnel: %v (Status: %s, Body: %q)", err, resp.Status, string(body))
		}
		return fmt.Errorf("failed to connect to IAP WebSocket tunnel: %v", err)
	}
	defer func() { _ = wsConn.Close() }()
	slog.Debug("negotiated IAP tunnel websocket subprotocol", "subprotocol", resp.Header.Get("Sec-Websocket-Protocol"))

	iapConn := NewIapConn(wsConn)

	if err := iapConn.Handshake(); err != nil {
		return fmt.Errorf("IAP handshake failed: %w", err)
	}

	go func() {
		localConn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = localConn.Close() }()

		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()
			written, err := io.Copy(iapConn, localConn)
			slog.Debug("copied local SSH bytes to IAP tunnel", "bytes", written, "error", err)
		}()

		go func() {
			defer wg.Done()
			_, _ = io.Copy(localConn, iapConn)
		}()

		wg.Wait()
	}()

	sshUser := c.SSHUser
	if sshUser == "" {
		sshUser = "root"
	}
	sshHost := c.SSHHost
	if sshHost == "" {
		sshHost = "127.0.0.1"
	}
	strictHostKeyChecking := c.StrictHostKeyChecking
	if strictHostKeyChecking == "" {
		strictHostKeyChecking = "accept-new"
	}

	sshArgs := []string{
		"-p", localPort,
		"-i", privKeyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=" + strictHostKeyChecking,
	}
	if c.UserKnownHostsFile != "" {
		sshArgs = append(sshArgs, "-o", "UserKnownHostsFile="+c.UserKnownHostsFile)
	}
	if c.SSHDebug {
		sshArgs = append(sshArgs, "-v")
	}
	sshArgs = append(sshArgs, sshUser+"@"+sshHost)
	sshArgs = appendRemoteCommand(sshArgs, cmdArgs)

	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	sshCmd.Stdin = stdin
	sshCmd.Stdout = stdout
	authDetector := &authFailureDetector{}
	sshCmd.Stderr = io.MultiWriter(stderr, authDetector)

	slog.Debug("running SSH command", "user", sshUser, "host", sshHost, "port", localPort, "command_args", len(cmdArgs))
	err = sshCmd.Run()
	slog.Debug("SSH command finished", "error", err)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 255 && authDetector.Seen() {
			return fmt.Errorf("SSH command failed: %w: %w", errSSHAuthFailed, err)
		}
		return fmt.Errorf("SSH command failed: %w", err)
	}

	return nil
}

// sshAuthFailureMarker is what ssh prints to stderr when the server rejects
// public key authentication.
const sshAuthFailureMarker = "Permission denied (publickey"

// authFailureDetector watches ssh's stderr for sshAuthFailureMarker, keeping
// only enough trailing bytes to match it across write boundaries.
type authFailureDetector struct {
	mu   sync.Mutex
	tail []byte
	seen bool
}

func (d *authFailureDetector) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen {
		return len(p), nil
	}
	buf := append(d.tail, p...)
	if bytes.Contains(buf, []byte(sshAuthFailureMarker)) {
		d.seen = true
		d.tail = nil
		return len(p), nil
	}
	if keep := len(sshAuthFailureMarker) - 1; len(buf) > keep {
		buf = buf[len(buf)-keep:]
	}
	d.tail = append([]byte(nil), buf...)
	return len(p), nil
}

// Seen reports whether the marker was written.
func (d *authFailureDetector) Seen() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen
}

// tokenSource returns the connector's token source, building it on first
// use. A failed build is not cached.
func (c *IAPExecConnector) tokenSource() (tokenSource, error) {
	c.credsMu.Lock()
	defer c.credsMu.Unlock()
	if c.creds != nil {
		return c.creds, nil
	}
	detect := c.detectCredentials
	if detect == nil {
		detect = detectDefaultCredentials
	}
	creds, err := detect()
	if err != nil {
		return nil, err
	}
	c.creds = creds
	return creds, nil
}

func detectDefaultCredentials() (tokenSource, error) {
	creds, err := credentials.DetectDefault(&credentials.DetectOptions{})
	if err != nil {
		return nil, err
	}
	return creds, nil
}

// projectNumber resolves projectID to its project number, caching successful
// lookups per project ID.
func (c *IAPExecConnector) projectNumber(ctx context.Context, projectID string) (string, error) {
	c.mu.Lock()
	number, ok := c.projectNumbers[projectID]
	c.mu.Unlock()
	if ok {
		return number, nil
	}

	lookup := c.lookupProjectNumber
	if lookup == nil {
		lookup = fetchProjectNumber
	}
	number, err := lookup(ctx, projectID)
	if err != nil {
		return "", err
	}

	c.mu.Lock()
	if c.projectNumbers == nil {
		c.projectNumbers = make(map[string]string)
	}
	c.projectNumbers[projectID] = number
	c.mu.Unlock()
	return number, nil
}

func instanceShortID(instanceName string) string {
	if idx := strings.LastIndex(instanceName, "/"); idx != -1 {
		return instanceName[idx+1:]
	}
	return instanceName
}

func instanceResourceName(project, location, shortID string) string {
	return fmt.Sprintf("projects/%s/locations/%s/instances/%s", project, location, shortID)
}

// appendRemoteCommand appends the remote command to run as a single trailing
// element of sshArgs, or leaves sshArgs unchanged when cmdArgs is empty (the
// interactive/attach path, where ssh should start a login shell instead of
// running a command).
//
// ssh joins every argument that follows user@host with spaces into one
// string and hands it to the remote login shell, which re-parses it. Passing
// cmdArgs through as separate trailing args would let the remote shell
// re-split and re-interpret any element containing a space or shell
// metacharacter. Quoting each element and joining them into a single string
// here ensures the remote shell instead sees each original argument as one
// literal word.
func appendRemoteCommand(sshArgs []string, cmdArgs []string) []string {
	if len(cmdArgs) == 0 {
		return sshArgs
	}
	return append(sshArgs, quoteRemoteCommand(cmdArgs))
}

// quoteRemoteCommand joins cmdArgs into a single POSIX shell command string
// in which each original argument round-trips as one literal word once the
// remote shell parses it.
func quoteRemoteCommand(cmdArgs []string) string {
	quoted := make([]string, len(cmdArgs))
	for i, arg := range cmdArgs {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

// shellQuote wraps s in single quotes for POSIX shell parsing. Each embedded
// single quote is replaced by closing the quoted string, inserting a
// backslash immediately before a literal single quote, and reopening the
// quoted string. The result parses back to exactly s as one word.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func fetchProjectNumber(ctx context.Context, projectID string) (string, error) {
	client, err := resourcemanager.NewProjectsClient(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = client.Close() }()

	req := &resourcemanagerpb.GetProjectRequest{
		Name: fmt.Sprintf("projects/%s", projectID),
	}
	proj, err := client.GetProject(ctx, req)
	if err != nil {
		return "", err
	}

	parts := strings.Split(proj.GetName(), "/")
	if len(parts) == 2 {
		return parts[1], nil
	}
	return "", fmt.Errorf("unexpected project name format: %s", proj.GetName())
}

func generateSSHKeyPair() ([]byte, []byte, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}

	privateKeyBytes := x509.MarshalPKCS1PrivateKey(privateKey)
	privKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: privateKeyBytes,
	})

	publicKey, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	pubKeyOpenSSH := ssh.MarshalAuthorizedKey(publicKey)

	return privKeyPEM, pubKeyOpenSSH, nil
}

// signedKeyForInstance returns an SSH key pair with an OS Login certificate
// for the named instance, signed for the service account the instance runs
// as. Expired signed keys are swept and the instance is looked up on every
// call; the call fails, and drops any signed key cached for the instance,
// when its service account cannot be determined. A signed key is reused for the same
// instance (name and uid) and service account while its certificate has at
// least signedKeyMinRemaining validity left.
func (c *IAPExecConnector) signedKeyForInstance(ctx context.Context, project, location, shortID, token string) (*signedKey, error) {
	name := instanceResourceName(project, location, shortID)
	now := c.clock()
	c.mu.Lock()
	c.sweepExpiredLocked(now)
	c.mu.Unlock()

	serviceAccount, uid, err := c.instanceServiceAccount(ctx, project, location, shortID)
	if err != nil {
		c.mu.Lock()
		delete(c.signedKeys, name)
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to determine service account of instance %q: %w", shortID, err)
	}

	// The lookup takes time, so expiry is checked again against a fresh
	// clock reading.
	c.mu.Lock()
	if k, ok := c.signedKeys[name]; ok && k.instanceUID == uid && k.serviceAccount == serviceAccount && c.clock().Before(k.reuseUntil) {
		c.mu.Unlock()
		return k, nil
	}
	c.mu.Unlock()

	privKeyPEM, pubKeyOpenSSH, err := generateSSHKeyPair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate SSH keys: %v", err)
	}
	signedCert, err := requestSignedCertificate(ctx, project, location, shortID, serviceAccount, string(pubKeyOpenSSH), token)
	if err != nil {
		return nil, fmt.Errorf("failed to sign SSH key: %v", err)
	}

	now = c.clock()
	k := &signedKey{
		instanceUID:    uid,
		serviceAccount: serviceAccount,
		privKeyPEM:     privKeyPEM,
		cert:           signedCert,
		reuseUntil:     certExpiry(signedCert, now).Add(-signedKeyMinRemaining),
	}
	if k.reuseUntil.After(now) {
		c.mu.Lock()
		if c.signedKeys == nil {
			c.signedKeys = make(map[string]*signedKey)
		}
		c.signedKeys[name] = k
		c.mu.Unlock()
	}
	return k, nil
}

// instanceServiceAccount returns the service account and uid of the named
// instance, read from the Instances API. It fails when the instance reports
// no service account.
func (c *IAPExecConnector) instanceServiceAccount(ctx context.Context, project, location, shortID string) (string, string, error) {
	inst, err := c.getInstance(ctx, project, location, shortID)
	if err != nil {
		return "", "", err
	}
	if inst.GetServiceAccount() == "" {
		return "", "", errors.New("instance reports no service account")
	}
	return inst.GetServiceAccount(), inst.GetUid(), nil
}

// getInstance reads the named instance from the Instances API.
func (c *IAPExecConnector) getInstance(ctx context.Context, project, location, shortID string) (*runpb.Instance, error) {
	client, err := c.instancesAPI(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create Instances API client: %w", err)
	}

	inst, err := client.GetInstance(ctx, &runpb.GetInstanceRequest{Name: instanceResourceName(project, location, shortID)})
	if err != nil {
		return nil, fmt.Errorf("failed to get instance: %w", err)
	}
	return inst, nil
}

// instancesAPI returns the connector's Instances API client, creating it on
// first use. The client outlives the call that creates it, so it is created
// with ctx's values but without its cancellation. A failed creation is not
// cached.
func (c *IAPExecConnector) instancesAPI(ctx context.Context) (InstancesAPI, error) {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.instancesClient != nil {
		return c.instancesClient, nil
	}
	newClient := c.newInstancesClient
	if newClient == nil {
		newClient = NewInstancesClient
	}
	client, err := newClient(context.WithoutCancel(ctx))
	if err != nil {
		return nil, err
	}
	c.instancesClient = client
	return client, nil
}

// Close releases the connector's Instances API client, if one was created.
// The cached client lives as long as the connector, which for the Cloud Run
// runtime is the lifetime of the process. Close exists for callers that own a
// shorter-lived connector.
func (c *IAPExecConnector) Close() error {
	c.clientMu.Lock()
	defer c.clientMu.Unlock()
	if c.instancesClient == nil {
		return nil
	}
	err := c.instancesClient.Close()
	c.instancesClient = nil
	return err
}

// sweepExpiredLocked drops cached signed keys that may no longer be reused.
// c.mu must be held.
func (c *IAPExecConnector) sweepExpiredLocked(now time.Time) {
	for name, k := range c.signedKeys {
		if !now.Before(k.reuseUntil) {
			delete(c.signedKeys, name)
		}
	}
}

func (c *IAPExecConnector) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// certExpiry returns when the OpenSSH certificate stops being valid, or
// signedKeyDefaultValidity after now when that cannot be read.
func certExpiry(cert string, now time.Time) time.Time {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cert))
	if err == nil {
		if sshCert, ok := pub.(*ssh.Certificate); ok && sshCert.ValidBefore != ssh.CertTimeInfinity && sshCert.ValidBefore <= math.MaxInt64 {
			return time.Unix(int64(sshCert.ValidBefore), 0)
		}
	}
	return now.Add(signedKeyDefaultValidity)
}

func requestSignedCertificate(ctx context.Context, projectID, region, instanceID, serviceAccount, publicKey, token string) (string, error) {
	url := fmt.Sprintf("%s/v1beta/projects/%s/locations/%s:signSshPublicKey", osLoginBaseURL, projectID, region)

	bodyData := map[string]string{
		"sshPublicKey":     publicKey,
		"serviceAccount":   serviceAccount,
		"cloudRunResource": fmt.Sprintf("projects/%s/locations/%s/instances/%s", projectID, region, instanceID),
	}
	jsonBody, err := json.Marshal(bodyData)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(jsonBody))
	if err != nil {
		return "", err
	}
	req.Header.Add("Authorization", "Bearer "+token)
	req.Header.Add("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("OS Login API returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var resData struct {
		SignedSshPublicKey string `json:"signedSshPublicKey"`
	}
	err = json.NewDecoder(resp.Body).Decode(&resData)
	if err != nil {
		return "", err
	}

	return resData.SignedSshPublicKey, nil
}

type IapConn struct {
	ws            *websocket.Conn
	bytesReceived uint64
	readBuffer    bytes.Buffer
	readMutex     sync.Mutex
	writeMutex    sync.Mutex
}

func NewIapConn(ws *websocket.Conn) *IapConn {
	return &IapConn{
		ws: ws,
	}
}

func (c *IapConn) Write(b []byte) (int, error) {
	c.writeMutex.Lock()
	defer c.writeMutex.Unlock()

	buf := new(bytes.Buffer)
	_ = binary.Write(buf, binary.BigEndian, uint16(0x0004))
	_ = binary.Write(buf, binary.BigEndian, uint32(len(b)))
	buf.Write(b)

	err := c.ws.WriteMessage(websocket.BinaryMessage, buf.Bytes())
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

func (c *IapConn) Read(b []byte) (int, error) {
	c.readMutex.Lock()
	defer c.readMutex.Unlock()

	for c.readBuffer.Len() == 0 {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			return 0, err
		}
		slog.Debug("received IAP tunnel websocket message", "message_type", mt, "length", len(msg))
		if mt != websocket.BinaryMessage {
			continue
		}

		if len(msg) < 2 {
			continue
		}

		tag := binary.BigEndian.Uint16(msg[0:2])
		payload := msg[2:]
		slog.Debug("received IAP tunnel frame", "tag", tag, "payload_length", len(payload))

		switch tag {
		case 0x0001:
		case 0x0002:
		case 0x0004:
			if len(payload) < 4 {
				continue
			}
			dataLen := binary.BigEndian.Uint32(payload[0:4])
			data := payload[4:]
			if uint32(len(data)) < dataLen {
				continue
			}

			c.readBuffer.Write(data[:dataLen])
			c.bytesReceived += uint64(dataLen)

			ackBuf := new(bytes.Buffer)
			_ = binary.Write(ackBuf, binary.BigEndian, uint16(0x0007))
			_ = binary.Write(ackBuf, binary.BigEndian, uint64(c.bytesReceived))
			c.writeMutex.Lock()
			_ = c.ws.WriteMessage(websocket.BinaryMessage, ackBuf.Bytes())
			c.writeMutex.Unlock()

		case 0x0007:
		}
	}

	return c.readBuffer.Read(b)
}

func (c *IapConn) Close() error {
	return c.ws.Close()
}

func (c *IapConn) LocalAddr() net.Addr {
	return c.ws.LocalAddr()
}

func (c *IapConn) RemoteAddr() net.Addr {
	return c.ws.RemoteAddr()
}

func (c *IapConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *IapConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *IapConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func (c *IapConn) Handshake() error {
	c.readMutex.Lock()
	defer c.readMutex.Unlock()

	for {
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message error: %w", err)
		}
		if mt != websocket.BinaryMessage {
			continue
		}
		if len(msg) < 2 {
			continue
		}
		tag := binary.BigEndian.Uint16(msg[0:2])
		if tag == 0x0001 || tag == 0x0002 {
			return nil
		}
	}
}
