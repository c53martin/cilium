// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package auth

import (
	"fmt"
	"net"
	"time"

	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/monitor"
	"github.com/cilium/cilium/pkg/policy"
)

type authManager struct {
	ipCache               ipCache
	authHandlers          map[policy.AuthType]authHandler
	datapathAuthenticator datapathAuthenticator
}

// ipCache is the set of interactions the auth manager performs with the IPCache
type ipCache interface {
	GetHostIP(ip string) net.IP
}

// authHandler is responsible to handle authentication for a specific auth type
type authHandler interface {
	authenticate(*authRequest) (*authResponse, error)
	authType() policy.AuthType
}

type authRequest struct {
	srcIdentity identity.NumericIdentity
	dstIdentity identity.NumericIdentity
	srcHostIP   net.IP
	dstHostIP   net.IP
}

type authResponse struct {
	expiryTime time.Time
}

// datapathAuthenticator is responsible to write auth information back to a BPF map
type datapathAuthenticator interface {
	markAuthenticated(dn *monitor.DropNotify, ci *monitor.ConnectionInfo, resp *authResponse) error
}

func newAuthManager(authHandlers []authHandler, dpAuthenticator datapathAuthenticator, ipCache ipCache) (*authManager, error) {
	ahs := map[policy.AuthType]authHandler{}
	for _, ah := range authHandlers {
		if _, ok := ahs[ah.authType()]; ok {
			return nil, fmt.Errorf("multiple handlers for auth type: %s", ah.authType())
		}
		ahs[ah.authType()] = ah
	}

	return &authManager{
		authHandlers:          ahs,
		datapathAuthenticator: dpAuthenticator,
		ipCache:               ipCache,
	}, nil
}

func (a *authManager) AuthRequired(dn *monitor.DropNotify, ci *monitor.ConnectionInfo) {
	if err := a.authRequired(dn, ci); err != nil {
		log.WithError(err).Warning("auth: Failed to authenticate request")
		return
	}
}

func (a *authManager) authRequired(dn *monitor.DropNotify, ci *monitor.ConnectionInfo) error {
	authType := getAuthType(dn)
	policyType := getPolicyType(dn)

	log.Debugf("auth: %s Policy is requiring authentication type %s for identity %d->%d, endpoint %s->%s",
		policyType, authType, dn.SrcLabel, dn.DstLabel, ci.SrcIP, ci.DstIP)

	// Authenticate according to the requested auth type
	h, ok := a.authHandlers[authType]
	if !ok {
		return fmt.Errorf("unknown requested auth type: %s", authType)
	}

	authReq, err := a.buildAuthRequest(dn, ci)
	if err != nil {
		return fmt.Errorf("failed to gather auth request information: %w", err)
	}

	authResp, err := h.authenticate(authReq)
	if err != nil {
		return fmt.Errorf("failed to authenticate with auth type %s: %w", authType, err)
	}

	if err := a.datapathAuthenticator.markAuthenticated(dn, ci, authResp); err != nil {
		return fmt.Errorf("failed to write auth information to BPF map: %w", err)
	}

	log.Debugf("auth: Successfully authenticated request for identity %s->%s, endpoint %s->%s, host %s->%s",
		dn.SrcLabel, dn.DstLabel, ci.SrcIP, ci.DstIP, authReq.srcHostIP, authReq.dstHostIP)

	return nil
}

func (a *authManager) buildAuthRequest(dn *monitor.DropNotify, ci *monitor.ConnectionInfo) (*authRequest, error) {
	srcHostIP := a.hostIPForConnIP(ci.SrcIP)
	if srcHostIP == nil {
		return nil, fmt.Errorf("failed to get host IP of connection source IP %s", ci.SrcIP)
	}

	dstHostIP := a.hostIPForConnIP(ci.DstIP)
	if dstHostIP == nil {
		return nil, fmt.Errorf("failed to get host IP of connection destination IP %s", ci.DstIP)

	}

	return &authRequest{
		srcIdentity: dn.SrcLabel,
		dstIdentity: dn.DstLabel,
		srcHostIP:   srcHostIP,
		dstHostIP:   dstHostIP,
	}, nil
}

func (a *authManager) hostIPForConnIP(connIP net.IP) net.IP {
	hostIP := a.ipCache.GetHostIP(connIP.String())
	if hostIP != nil {
		return hostIP
	}

	// Checking for Cilium's internal IP (cilium_host).
	// This might be the case when checking ingress auth after egress L7 policies are applied and therefore traffic
	// gets rerouted via Cilium's envoy proxy.
	if ip.IsIPv4(connIP) {
		return a.ipCache.GetHostIP(fmt.Sprintf("%s/32", connIP))
	} else if ip.IsIPv6(connIP) {
		return a.ipCache.GetHostIP(fmt.Sprintf("%s/128", connIP))
	}

	return nil
}

func isIngress(dn *monitor.DropNotify) bool {
	// DropNotify.DstID is 0 for egress, non-zero for Ingress
	return dn.DstID != 0
}

func getAuthType(dn *monitor.DropNotify) policy.AuthType {
	// Requested authentication type is in DropNotify.ExtError field
	return policy.AuthType(dn.ExtError)
}

func getPolicyType(dn *monitor.DropNotify) string {
	if isIngress(dn) {
		return "Ingress"
	}
	return "Egress"
}
