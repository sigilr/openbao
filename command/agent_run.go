// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hashicorp/cli"
	remotedb "github.com/openbao/openbao/plugins/database/remote-db-plugin"
	proto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto/gen"
	"github.com/openbao/openbao/plugins/database/remote-db-plugin/runner"
	"github.com/posener/complete"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// AgentRunCommand is the long-running spoke daemon. It connects to the hub's
// gRPC proxy port using mTLS (with credentials produced by `bao agent join`),
// dispatches inbound requests to a long-lived in-process plugin runner, and
// sends periodic heartbeats so the hub can mark the spoke healthy in
// `bao agent list`.
//
// The certificate's Common Name is the spoke's authoritative identity; the
// hub reads it off the verified peer cert. Concurrent in-flight requests are
// matched to responses via the AgentMessage.RequestId field and dispatched on
// independent goroutines so a slow plugin call never blocks others.
type AgentRunCommand struct {
	*BaseCommand

	flagServer            string
	flagCredentialsDir    string
	flagServerName        string
	flagHeartbeatInterval time.Duration
	flagMaxConcurrency    int
	flagRenewCheckEvery   time.Duration
	flagRenewThreshold    float64
}

var (
	_ cli.Command             = (*AgentRunCommand)(nil)
	_ cli.CommandAutocomplete = (*AgentRunCommand)(nil)
)

func (c *AgentRunCommand) Synopsis() string {
	return "Run the spoke daemon (connects to a hub and serves DB plugin requests)"
}

func (c *AgentRunCommand) Help() string {
	helpText := `
Usage: bao agent run [options]

  Long-running spoke daemon. Connects to a hub OpenBao's proxy gRPC port
  using the credentials produced by 'bao agent join', then serves database
  plugin requests in-process against locally-reachable databases.

  The credentials directory must contain:

      cert.pem    client cert issued by 'bao agent join'
      key.pem     matching private key
      ca.pem      spoke-CA root used to verify the hub

  Example:

      $ bao agent run \
          -server=hub.example.com:50053 \
          -credentials-dir=/etc/openbao-spoke

` + c.Flags().Help()
	return strings.TrimSpace(helpText)
}

func (c *AgentRunCommand) Flags() *FlagSets {
	set := c.flagSet(FlagSetNone)
	f := set.NewFlagSet("Command Options")

	f.StringVar(&StringVar{
		Name:    "server",
		Target:  &c.flagServer,
		Default: "localhost:50053",
		Usage:   "Hub gRPC address (host:port).",
	})
	f.StringVar(&StringVar{
		Name:    "credentials-dir",
		Target:  &c.flagCredentialsDir,
		Default: "/etc/openbao-spoke",
		Usage:   "Directory containing cert.pem, key.pem, ca.pem.",
	})
	f.StringVar(&StringVar{
		Name:    "server-name",
		Target:  &c.flagServerName,
		Default: "",
		Usage:   "Override SNI / expected hub CN (defaults to the host part of -server).",
	})
	f.DurationVar(&DurationVar{
		Name:    "heartbeat-interval",
		Target:  &c.flagHeartbeatInterval,
		Default: 15 * time.Second,
		Usage:   "Liveness heartbeat cadence. 0 disables.",
	})
	f.IntVar(&IntVar{
		Name:    "max-concurrency",
		Target:  &c.flagMaxConcurrency,
		Default: 32,
		Usage:   "Max concurrent in-flight requests from the hub.",
	})
	f.DurationVar(&DurationVar{
		Name:    "renew-check-every",
		Target:  &c.flagRenewCheckEvery,
		Default: time.Hour,
		Usage:   "How often to check whether the client cert is past its renewal threshold. 0 disables auto-renewal.",
	})
	f.Float64Var(&Float64Var{
		Name:    "renew-threshold",
		Target:  &c.flagRenewThreshold,
		Default: 0.5,
		Usage:   "Renew when this fraction of the cert lifetime has elapsed (0.5 = half-life).",
	})
	return set
}

func (c *AgentRunCommand) AutocompleteArgs() complete.Predictor { return nil }
func (c *AgentRunCommand) AutocompleteFlags() complete.Flags    { return c.Flags().Completions() }

func (c *AgentRunCommand) Run(args []string) int {
	if err := c.Flags().Parse(args); err != nil {
		c.UI.Error(err.Error())
		return 1
	}

	tlsCfg, err := loadSpokeTLS(c.flagCredentialsDir, c.flagServerName, c.flagServer)
	if err != nil {
		c.UI.Error(fmt.Sprintf("tls: %s", err))
		return 1
	}
	spokeName := tlsCfg.Certificates[0].Leaf.Subject.CommonName
	c.UI.Info(fmt.Sprintf("connecting to hub as spoke %q", spokeName))

	conn, err := grpc.NewClient(
		c.flagServer,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(remotedb.MaxMessageBytes),
			grpc.MaxCallSendMsgSize(remotedb.MaxMessageBytes),
		),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		c.UI.Error(fmt.Sprintf("dial: %s", err))
		return 1
	}
	defer func() { _ = conn.Close() }()

	stream, err := proto.NewAgentServiceClient(conn).Connect(context.Background())
	if err != nil {
		c.UI.Error(fmt.Sprintf("open stream: %s", err))
		return 1
	}

	// hbCtx scopes the heartbeat + renewal goroutines. We also cancel it
	// from the sender goroutine when Send fails, so those goroutines exit
	// promptly rather than blindly feeding messages into a drain loop while
	// the daemon hasn't yet noticed the stream is dead.
	hbCtx, cancelHB := context.WithCancel(context.Background())
	defer cancelHB()

	// stream.Send is not safe for concurrent calls. The previous mutex-based
	// serialization made the heartbeat goroutine block (holding sendMu)
	// whenever the request handlers were also sending — the same lock the
	// hub uses to detect a wedged spoke. Move to a sendCh + dedicated
	// sender goroutine so a slow Send only backs up the queue, never the
	// heartbeat ticker. The hub side uses the same pattern (proxy.go).
	sendCh := make(chan *proto.AgentMessage, 64)
	sendDone := make(chan struct{})
	sendErrCh := make(chan error, 1)
	go func() {
		defer close(sendDone)
		for msg := range sendCh {
			if err := stream.Send(msg); err != nil {
				select {
				case sendErrCh <- err:
				default:
				}
				// Cancel the heartbeat/renewal context so those goroutines
				// stop posting into sendCh — otherwise they keep firing
				// messages that the drain below just throws away, and the
				// hub keeps marking the spoke healthy for up to one
				// keepalive timeout while we're already failed.
				cancelHB()
				// Drain the rest so producers don't block forever; we will
				// exit shortly after the recv loop also notices.
				for range sendCh {
				}
				return
			}
		}
	}()
	send := func(msg *proto.AgentMessage) error {
		select {
		case sendCh <- msg:
			return nil
		case <-sendDone:
			return fmt.Errorf("send: stream closed")
		}
	}
	defer func() {
		close(sendCh)
		<-sendDone
	}()

	if err := send(&proto.AgentMessage{ClientName: spokeName, IsResponse: false}); err != nil {
		c.UI.Error(fmt.Sprintf("register: %s", err))
		return 1
	}
	ack, err := stream.Recv()
	if err != nil {
		c.UI.Error(fmt.Sprintf("recv ack: %s", err))
		return 1
	}
	c.UI.Info(fmt.Sprintf("registered: %s", ack.Output))

	if c.flagHeartbeatInterval > 0 {
		go runSpokeHeartbeat(hbCtx, send, spokeName, c.flagHeartbeatInterval, c.UI)
	}
	if c.flagRenewCheckEvery > 0 {
		go runCertRenewal(hbCtx, RenewSpokeCertInput{
			Server:         c.flagServer,
			ServerName:     c.flagServerName,
			CredentialsDir: c.flagCredentialsDir,
		}, c.flagRenewCheckEvery, c.flagRenewThreshold, c.UI)
	}

	r := runner.NewPluginRunner()
	// Evict cached plugin instances that haven't been touched within the TTL.
	// Catches mounts the hub forgot to Close (process crash, deleted while
	// the spoke was offline). hbCtx cancels on stream teardown so the
	// evictor exits with the daemon.
	r.StartIdleEvictor(hbCtx)

	// Worker pool bounds concurrency. Each inbound request is dispatched on
	// a worker; the request_id flows back on the response so the hub can
	// match it to its waiter.
	sem := make(chan struct{}, c.flagMaxConcurrency)

	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			c.UI.Info("hub disconnected")
			return 0
		}
		if err != nil {
			c.UI.Error(fmt.Sprintf("stream error: %s", err))
			return 1
		}
		select {
		case sendErr := <-sendErrCh:
			c.UI.Error(fmt.Sprintf("send loop: %s", sendErr))
			return 1
		default:
		}
		// Heartbeats and the initial Connected ack don't carry work.
		if msg.Command == "" || msg.IsResponse {
			continue
		}

		sem <- struct{}{}
		go func(m *proto.AgentMessage) {
			defer func() { <-sem }()
			output, execErr := r.ExecuteRequest(m.Command)
			resp := &proto.AgentMessage{
				ClientName: spokeName,
				RequestId:  m.RequestId,
				IsResponse: true,
			}
			if execErr != nil {
				resp.Error = execErr.Error()
			} else {
				resp.Output = output
			}
			if err := send(resp); err != nil {
				c.UI.Error(fmt.Sprintf("send response (req %s): %s", m.RequestId, err))
			}
		}(msg)
	}
}

// runCertRenewal ticks on `every` and renews the spoke's client cert once it
// passes the threshold fraction of its lifetime. Failures are logged and
// retried on the next tick — there is no point bailing out of the daemon
// because the hub is briefly unreachable. The renewed cert is written to
// disk; in-flight gRPC connections stay on the old cert until they
// reconnect, which is fine if renewal runs with a sensible threshold
// (default 0.5).
func runCertRenewal(ctx context.Context, in RenewSpokeCertInput, every time.Duration, threshold float64, ui cli.Ui) {
	t := time.NewTicker(every)
	defer t.Stop()
	// Check once at startup; an operator restarting a daemon with an
	// almost-expired cert shouldn't have to wait for the first tick.
	maybeRenew(ctx, in, threshold, ui)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			maybeRenew(ctx, in, threshold, ui)
		}
	}
}

func maybeRenew(ctx context.Context, in RenewSpokeCertInput, threshold float64, ui cli.Ui) {
	notBefore, notAfter, err := CurrentSpokeCertWindow(in.CredentialsDir)
	if err != nil {
		ui.Error(fmt.Sprintf("renew: read cert: %s", err))
		return
	}
	if !PastRenewalThreshold(notBefore, notAfter, threshold, time.Now()) {
		return
	}
	res, err := RenewSpokeCert(ctx, in)
	if err != nil {
		ui.Error(fmt.Sprintf("renew: %s", err))
		return
	}
	ui.Info(fmt.Sprintf("renewed cert for %q; new expiry %s", res.CommonName,
		res.NotAfter.UTC().Format(time.RFC3339)))
}

// runSpokeHeartbeat fires an IsHeartbeat frame every interval. Hub side
// increments its last-seen timestamp on receipt; the spoke considers itself
// dead when the stream errors out (Send will report and we just stop ticking).
func runSpokeHeartbeat(ctx context.Context, send func(*proto.AgentMessage) error, spokeName string, interval time.Duration, ui cli.Ui) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := send(&proto.AgentMessage{
				ClientName:  spokeName,
				IsHeartbeat: true,
			}); err != nil {
				ui.Error(fmt.Sprintf("heartbeat: %s", err))
				return
			}
		}
	}
}

// loadSpokeTLS reads cert/key/ca from credsDir and returns a tls.Config
// suitable for grpc.NewClient. The leaf cert is parsed so the CN is available
// as the spoke identity without a second open of the PEM file.
func loadSpokeTLS(credsDir, serverName, serverAddr string) (*tls.Config, error) {
	certPath := filepath.Join(credsDir, "cert.pem")
	keyPath := filepath.Join(credsDir, "key.pem")
	caPath := filepath.Join(credsDir, "ca.pem")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load client cert/key from %s: %w", credsDir, err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse client cert: %w", err)
	}
	cert.Leaf = leaf

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s did not yield any CA certs", caPath)
	}

	if serverName == "" {
		host, _, _ := strings.Cut(serverAddr, ":")
		serverName = host
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}, nil
}
