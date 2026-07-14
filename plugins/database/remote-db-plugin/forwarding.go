// Copyright (c) AppsCode Inc.
// SPDX-License-Identifier: MPL-2.0

package remotedb

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/openbao/openbao/plugins/database/remote-db-plugin/bootstrap"
	agentproto "github.com/openbao/openbao/plugins/database/remote-db-plugin/proto/gen"
	"github.com/openbao/openbao/vault/relayfwd"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// relayForwardingCallTimeout bounds a single cross-node forward (dial + call).
// A credential operation must not hang on a wedged peer.
const relayForwardingCallTimeout = 10 * time.Second

// SetRelayNode wires the given Core's HA view into the proxy server bound to
// that node and starts its spoke announcer. Called by the relay backend on every
// unseal (both active and standby, since hydrateHubState runs under the
// read-only strategy too), and directly by HA tests. Idempotent: a later call
// just replaces the node view and leaves the running announcer in place.
//
// On a single-node hub this is still called, but node.IsActive() is always true
// and the announcer is a no-op (the active node announces nothing), so the hot
// path stays local-only.
func SetRelayNode(node relayfwd.Node) {
	s := proxyServerForNode(node)
	s.nodeMu.Lock()
	s.node = node
	if s.registry == nil {
		s.registry = newSpokeRegistry(announceInterval)
	}
	s.nodeMu.Unlock()

	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	if s.announcerStop == nil {
		stop := make(chan struct{})
		s.announcerStop = stop
		go s.runAnnouncer(stop)
	}
}

// runAnnouncer periodically pushes this node's full local spoke set to the
// active node, and also re-announces immediately when it observes a leadership
// change. See DESIGN.md "The spoke registry is built by announcement".
func (s *proxyServer) runAnnouncer(stop <-chan struct{}) {
	ticker := time.NewTicker(announceInterval)
	defer ticker.Stop()
	lastLeader := ""
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			leader := s.announceOnce()
			// The 5s tick already bounds propagation to one announce interval;
			// re-announcing the instant the leader changes closes the gap on
			// the case that matters most (failover). announceOnce returns the
			// leader it targeted so we can detect the change cheaply.
			if leader != "" && leader != lastLeader {
				lastLeader = leader
				s.announceOnce()
			}
		}
	}
}

// announceOnce sends one full announcement to the active node. Returns the
// leader cluster address it targeted (empty when it did not announce: this node
// is active, sealed, or leadership is unknown), so the caller can detect
// leadership changes.
func (s *proxyServer) announceOnce() string {
	node := s.getNode()
	if node == nil || node.Sealed() || node.IsActive() {
		return ""
	}
	leader, err := node.LeaderClusterAddr()
	if err != nil || leader == "" {
		return ""
	}

	spokes := s.localAnnouncedSpokes()
	ctx, cancel := context.WithTimeout(context.Background(), relayForwardingCallTimeout)
	defer cancel()

	conn, err := node.DialForwarding(ctx, leader)
	if err != nil {
		log.Printf("[relay-fwd] announce dial to leader %s failed: %v", leader, err)
		return leader
	}
	defer conn.Close()

	client := agentproto.NewRelayForwardingClient(conn)
	_, err = client.AnnounceSpokes(ctx, &agentproto.AnnounceSpokesRequest{
		NodeClusterAddr: node.ClusterAddr(),
		NodeId:          node.NodeID(),
		Spokes:          toProtoSpokes(spokes),
	})
	if err != nil {
		// FailedPrecondition means the target is no longer active; the next
		// tick re-resolves the leader via Core.Leader() and re-announces. Any
		// other error is transient and self-heals the same way.
		if status.Code(err) != codes.FailedPrecondition {
			log.Printf("[relay-fwd] announce to leader %s failed: %v", leader, err)
		}
	}
	return leader
}

// localAnnouncedSpokes snapshots the spokes whose streams terminate on this
// node, for announcement to the active node.
func (s *proxyServer) localAnnouncedSpokes() []AnnouncedSpoke {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AnnouncedSpoke, 0, len(s.spokes))
	for name, c := range s.spokes {
		out = append(out, AnnouncedSpoke{
			SpokeName:    name,
			ConnectedAt:  c.connectedAt,
			LastSeen:     c.lastSeenAt(),
			CertNotAfter: c.certNotAfterAt(),
		})
	}
	return out
}

func toProtoSpokes(in []AnnouncedSpoke) []*agentproto.SpokeEntry {
	out := make([]*agentproto.SpokeEntry, 0, len(in))
	for _, s := range in {
		e := &agentproto.SpokeEntry{
			SpokeName:       s.SpokeName,
			ConnectedAtUnix: s.ConnectedAt.Unix(),
			LastSeenUnix:    s.LastSeen.Unix(),
		}
		if !s.CertNotAfter.IsZero() {
			e.CertNotAfter = s.CertNotAfter.Unix()
		}
		out = append(out, e)
	}
	return out
}

func fromProtoSpokes(in []*agentproto.SpokeEntry) []AnnouncedSpoke {
	out := make([]AnnouncedSpoke, 0, len(in))
	for _, e := range in {
		s := AnnouncedSpoke{
			SpokeName:   e.SpokeName,
			ConnectedAt: time.Unix(e.ConnectedAtUnix, 0),
			LastSeen:    time.Unix(e.LastSeenUnix, 0),
		}
		if e.CertNotAfter != 0 {
			s.CertNotAfter = time.Unix(e.CertNotAfter, 0)
		}
		out = append(out, s)
	}
	return out
}

// forwardRunCommand forwards a credential command to the node that holds the
// spoke stream and returns its result. The peer runs its own identical local
// path (runLocalOnly), so the spoke never learns its frames took an extra hop.
func (s *proxyServer) forwardRunCommand(ctx context.Context, node relayfwd.Node, loc SpokeLocation, spokeName, command string) (string, error) {
	conn, err := node.DialForwarding(ctx, loc.NodeClusterAddr)
	if err != nil {
		return "", fmt.Errorf("cannot reach spoke owner %s: %w", loc.NodeClusterAddr, err)
	}
	defer conn.Close()

	client := agentproto.NewRelayForwardingClient(conn)
	resp, err := client.RunCommand(ctx, &agentproto.RelayRunCommandRequest{
		SpokeName: spokeName,
		Command:   command,
	})
	if err != nil {
		return "", err
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Output, nil
}

// forwardRenewCert forwards a spoke's renewal CSR to the active node for
// signing and returns the signed cert. The standby holding the stream records
// the fresh NotAfter on its own spokeConnection (under the same lock as
// RenewCert's local path) and returns the cert to the spoke over the existing
// stream, so the spoke sees an ordinary renewal.
func (s *proxyServer) forwardRenewCert(ctx context.Context, node relayfwd.Node, peerCN string, req *agentproto.RenewCertRequest) (*agentproto.RenewCertResponse, error) {
	leader, err := node.LeaderClusterAddr()
	if err != nil || leader == "" {
		return nil, fmt.Errorf("cannot resolve active node for cert signing: %v", err)
	}
	dctx, cancel := context.WithTimeout(ctx, relayForwardingCallTimeout)
	defer cancel()

	conn, err := node.DialForwarding(dctx, leader)
	if err != nil {
		return nil, fmt.Errorf("cannot reach active node %s for cert signing: %w", leader, err)
	}
	defer conn.Close()

	client := agentproto.NewRelayForwardingClient(conn)
	resp, err := client.SignSpokeCSR(dctx, &agentproto.RelaySignCSRRequest{
		CsrPem:     req.CsrPem,
		SpokeName:  peerCN,
		TtlSeconds: req.TtlSeconds,
	})
	if err != nil {
		return nil, err
	}

	// Renewal happens in place over the live stream (the spoke does not
	// reconnect), so refresh the connection's recorded expiry from the fresh
	// cert. Best-effort, mirroring the active-node path in RenewCert.
	if newNotAfter, perr := certNotAfterFromPEM(resp.CertPem); perr == nil {
		s.mu.RLock()
		sc, ok := s.spokes[peerCN]
		s.mu.RUnlock()
		if ok {
			sc.setCertNotAfter(newNotAfter)
		}
	}

	return &agentproto.RenewCertResponse{
		CertPem:   resp.CertPem,
		CaCertPem: resp.CaCertPem,
	}, nil
}

// runLocalOnly looks up the spoke in the local map and runs the command over its
// stream, never forwarding. It is what the RelayForwarding.RunCommand handler
// calls on the node that terminates the stream, so a forwarded frame cannot hop
// again and loop.
func (s *proxyServer) runLocalOnly(ctx context.Context, spokeName, command string) (string, error) {
	s.mu.RLock()
	conn, ok := s.spokes[spokeName]
	s.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("spoke %q not connected", spokeName)
	}
	return s.runLocalConn(ctx, conn, spokeName, command)
}

// isSpokeNotConnected reports whether err is the "spoke not connected" signal
// (from a stale registry entry pointing at a node that no longer holds the
// stream), so RunCommand can re-resolve exactly once.
func isSpokeNotConnected(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not connected")
}

// --- RelayForwarding gRPC service (rides the cluster port) -------------------

// relayForwardingService implements agentproto.RelayForwardingServer. It is a
// distinct type from proxyServer only because proxyServer already has a
// RunCommand method with a different signature; both delegate to the same
// proxyServer state.
type relayForwardingService struct {
	agentproto.UnimplementedRelayForwardingServer
	s *proxyServer
}

// RelayForwardingService returns the default instance's service implementation.
func RelayForwardingService() agentproto.RelayForwardingServer {
	return &relayForwardingService{s: proxyServerInstance}
}

// RelayForwardingServiceForNode returns the service implementation backed by the
// proxy server bound to the given node. The vault-side ALPN handler registers
// this so a forwarded RunCommand lands on the right node's spoke map.
func RelayForwardingServiceForNode(node relayfwd.Node) agentproto.RelayForwardingServer {
	return &relayForwardingService{s: proxyServerForNode(node)}
}

// AnnounceSpokes records a peer's full local spoke set. Rejected unless this
// node is active, with the current leader in the message so the announcer can
// re-resolve and re-announce (prevents a demoted node from accumulating a
// phantom registry).
func (rf *relayForwardingService) AnnounceSpokes(ctx context.Context, req *agentproto.AnnounceSpokesRequest) (*agentproto.AnnounceSpokesResponse, error) {
	node := rf.s.getNode()
	if node == nil || !node.IsActive() {
		leader := ""
		if node != nil {
			leader, _ = node.LeaderClusterAddr()
		}
		return nil, status.Errorf(codes.FailedPrecondition, "node is not active; leader=%s", leader)
	}
	reg := rf.s.getRegistry()
	if reg == nil {
		return nil, status.Error(codes.FailedPrecondition, "relay registry not initialized")
	}
	reg.applyFullAnnounce(req.NodeClusterAddr, req.NodeId, fromProtoSpokes(req.Spokes))
	return &agentproto.AnnounceSpokesResponse{}, nil
}

// RunCommand executes a forwarded command against a spoke this node terminates.
// It never forwards again: if the spoke is not local, it returns the "not
// connected" signal so the active node re-resolves.
func (rf *relayForwardingService) RunCommand(ctx context.Context, req *agentproto.RelayRunCommandRequest) (*agentproto.RelayRunCommandResponse, error) {
	out, err := rf.s.runLocalOnly(ctx, req.SpokeName, req.Command)
	if err != nil {
		return &agentproto.RelayRunCommandResponse{Error: err.Error()}, nil
	}
	return &agentproto.RelayRunCommandResponse{Output: out}, nil
}

// SignSpokeCSR signs a spoke renewal CSR on the active node, keeping cert
// issuance a single-issuer authority operation even though the spoke-CA key is
// in memory on every unsealed node. The CN the cert is signed for is the peer
// cert CN the requesting node already authenticated (req.SpokeName), not
// whatever the CSR claims; a mismatch is rejected.
func (rf *relayForwardingService) SignSpokeCSR(ctx context.Context, req *agentproto.RelaySignCSRRequest) (*agentproto.RelaySignCSRResponse, error) {
	node := rf.s.getNode()
	if node == nil || !node.IsActive() {
		leader := ""
		if node != nil {
			leader, _ = node.LeaderClusterAddr()
		}
		return nil, status.Errorf(codes.FailedPrecondition, "node is not active; leader=%s", leader)
	}
	if req.SpokeName == "" {
		return nil, status.Error(codes.InvalidArgument, "spoke_name is required")
	}
	if len(req.CsrPem) == 0 {
		return nil, status.Error(codes.InvalidArgument, "csr_pem is required")
	}

	csrDER, err := bootstrap.DecodeCSRPEM(req.CsrPem)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse CSR: %v", err)
	}
	if csr.Subject.CommonName != req.SpokeName {
		return nil, status.Errorf(codes.InvalidArgument,
			"CSR CN %q does not match authenticated spoke %q", csr.Subject.CommonName, req.SpokeName)
	}

	caCertPEM, caKeyPEM := bootstrap.Global().CABundlePEM()
	if len(caCertPEM) == 0 || len(caKeyPEM) == 0 {
		return nil, status.Error(codes.FailedPrecondition, "hub identity not initialized")
	}
	ca := &bootstrap.CABundle{CertPEM: caCertPEM, KeyPEM: caKeyPEM}

	ttl := renewCertTTLFromSeconds(req.TtlSeconds)
	if ttl <= 0 {
		return nil, status.Errorf(codes.InvalidArgument, "ttl_seconds must be non-negative (got %d)", req.TtlSeconds)
	}
	certPEM, err := ca.SignSpokeCSR(csrDER, req.SpokeName, ttl)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "%v", err)
	}
	return &agentproto.RelaySignCSRResponse{
		CertPem:   certPEM,
		CaCertPem: caCertPEM,
	}, nil
}

// renewCertTTLFromSeconds clamps a requested TTL to the RenewCert bounds. Mirror
// of the arithmetic in RenewCert: clamp in seconds BEFORE multiplying by
// time.Second so a huge request cannot overflow into a negative duration.
// Returns a negative value only for a negative input (caller rejects it).
func renewCertTTLFromSeconds(seconds int64) time.Duration {
	const maxSeconds = int64(RenewCertMaxTTL / time.Second)
	switch {
	case seconds < 0:
		return -1
	case seconds == 0:
		return RenewCertDefaultTTL
	case seconds >= maxSeconds:
		return RenewCertMaxTTL
	default:
		return time.Duration(seconds) * time.Second
	}
}
