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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth/credentials"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
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
}

func NewIAPExecConnector(iapTunnelUrlOverride string) *IAPExecConnector {
	return &IAPExecConnector{
		IapTunnelUrlOverride: iapTunnelUrlOverride,
	}
}

func (c *IAPExecConnector) Connect(ctx context.Context, project, location, instanceName string) error {
	return c.runSSH(ctx, project, location, instanceName, nil, os.Stdin, os.Stdout, os.Stderr)
}

func (c *IAPExecConnector) Exec(ctx context.Context, project, location, instanceName string, cmd []string) ([]byte, error) {
	var outBuf bytes.Buffer
	err := c.runSSH(ctx, project, location, instanceName, cmd, nil, &outBuf, &outBuf)
	return outBuf.Bytes(), err
}

func (c *IAPExecConnector) runSSH(ctx context.Context, project, location, instanceName string, cmdArgs []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	authCreds, err := credentials.DetectDefault(&credentials.DetectOptions{})
	if err != nil {
		return fmt.Errorf("failed to detect credentials: %v", err)
	}
	tok, err := authCreds.Token(ctx)
	if err != nil {
		return fmt.Errorf("failed to retrieve auth token: %v", err)
	}

	projectNumber, err := fetchProjectNumber(ctx, project)
	if err != nil {
		return fmt.Errorf("failed to resolve project number: %v", err)
	}

	shortID := instanceName
	if idx := strings.LastIndex(instanceName, "/"); idx != -1 {
		shortID = instanceName[idx+1:]
	}

	privKeyPEM, pubKeyOpenSSH, err := generateSSHKeyPair()
	if err != nil {
		return fmt.Errorf("failed to generate SSH keys: %v", err)
	}

	tempDir, err := os.MkdirTemp("", "run-ssh-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %v", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	privKeyPath := filepath.Join(tempDir, "id_rsa")
	err = os.WriteFile(privKeyPath, privKeyPEM, 0600)
	if err != nil {
		return fmt.Errorf("failed to write private key: %v", err)
	}

	serviceAccount := fmt.Sprintf("%s-compute@developer.gserviceaccount.com", projectNumber)
	signedCert, err := requestSignedCertificate(ctx, project, location, shortID, serviceAccount, string(pubKeyOpenSSH), tok.Value)
	if err != nil {
		return fmt.Errorf("failed to sign SSH key: %v", err)
	}

	certPath := filepath.Join(tempDir, "id_rsa-cert.pub")
	err = os.WriteFile(certPath, []byte(signedCert), 0644)
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

	if len(cmdArgs) > 0 {
		sshArgs = append(sshArgs, cmdArgs...)
	}

	sshCmd := exec.CommandContext(ctx, "ssh", sshArgs...)
	sshCmd.Stdin = stdin
	sshCmd.Stdout = stdout
	sshCmd.Stderr = stderr

	slog.Debug("running SSH command", "user", sshUser, "host", sshHost, "port", localPort, "command_args", len(cmdArgs))
	err = sshCmd.Run()
	slog.Debug("SSH command finished", "error", err)
	if err != nil {
		return fmt.Errorf("SSH command failed: %w", err)
	}

	return nil
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

func requestSignedCertificate(ctx context.Context, projectID, region, instanceID, serviceAccount, publicKey, token string) (string, error) {
	url := fmt.Sprintf("https://oslogin.googleapis.com/v1beta/projects/%s/locations/%s:signSshPublicKey", projectID, region)

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
