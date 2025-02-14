// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package controlclient

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"sort"

	"tailscale.com/envknob"
	"tailscale.com/tailcfg"
	"tailscale.com/tstime"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/views"
	"tailscale.com/util/cmpx"
	"tailscale.com/wgengine/filter"
)

// mapSession holds the state over a long-polled "map" request to the
// control plane.
//
// It accepts incremental tailcfg.MapResponse values to
// netMapForResponse and returns fully inflated NetworkMaps, filling
// in the omitted data implicit from prior MapResponse values from
// within the same session (the same long-poll HTTP response to the
// one MapRequest).
type mapSession struct {
	// Immutable fields.
	nu             NetmapUpdater // called on changes (in addition to the optional hooks below)
	privateNodeKey key.NodePrivate
	logf           logger.Logf
	vlogf          logger.Logf
	machinePubKey  key.MachinePublic
	altClock       tstime.Clock       // if nil, regular time is used
	cancel         context.CancelFunc // always non-nil, shuts down caller's base long poll context
	watchdogReset  chan struct{}      // send to request that the long poll activity watchdog timeout be reset

	// sessionAliveCtx is a Background-based context that's alive for the
	// duration of the mapSession that we own the lifetime of. It's closed by
	// sessionAliveCtxClose.
	sessionAliveCtx      context.Context
	sessionAliveCtxClose context.CancelFunc // closes sessionAliveCtx

	// Optional hooks, set once before use.

	// onDebug specifies what to do with a *tailcfg.Debug message.
	// If the watchdogReset chan is nil, it's not used. Otherwise it can be sent to
	// to request that the long poll activity watchdog timeout be reset.
	onDebug func(_ context.Context, _ *tailcfg.Debug, watchdogReset chan<- struct{}) error

	// onConciseNetMapSummary, if non-nil, is called with the Netmap.VeryConcise summary
	// whenever a map response is received.
	onConciseNetMapSummary func(string)

	// onSelfNodeChanged is called before the NetmapUpdater if the self node was
	// changed.
	onSelfNodeChanged func(*netmap.NetworkMap)

	// Fields storing state over the course of multiple MapResponses.
	lastNode               tailcfg.NodeView
	lastDNSConfig          *tailcfg.DNSConfig
	lastDERPMap            *tailcfg.DERPMap
	lastUserProfile        map[tailcfg.UserID]tailcfg.UserProfile
	lastPacketFilterRules  views.Slice[tailcfg.FilterRule]
	lastParsedPacketFilter []filter.Match
	lastSSHPolicy          *tailcfg.SSHPolicy
	collectServices        bool
	previousPeers          []*tailcfg.Node // for delta-purposes
	lastDomain             string
	lastDomainAuditLogID   string
	lastHealth             []string
	lastPopBrowserURL      string
	stickyDebug            tailcfg.Debug // accumulated opt.Bool values
	lastTKAInfo            *tailcfg.TKAInfo
	lastNetmapSummary      string // from NetworkMap.VeryConcise

	// netMapBuilding is non-nil during a netmapForResponse call,
	// containing the value to be returned, once fully populated.
	netMapBuilding *netmap.NetworkMap
}

// newMapSession returns a mostly unconfigured new mapSession.
//
// Modify its optional fields on the returned value before use.
//
// It must have its Close method called to release resources.
func newMapSession(privateNodeKey key.NodePrivate, nu NetmapUpdater) *mapSession {
	ms := &mapSession{
		nu:              nu,
		privateNodeKey:  privateNodeKey,
		lastDNSConfig:   new(tailcfg.DNSConfig),
		lastUserProfile: map[tailcfg.UserID]tailcfg.UserProfile{},
		watchdogReset:   make(chan struct{}),

		// Non-nil no-op defaults, to be optionally overridden by the caller.
		logf:                   logger.Discard,
		vlogf:                  logger.Discard,
		cancel:                 func() {},
		onDebug:                func(context.Context, *tailcfg.Debug, chan<- struct{}) error { return nil },
		onConciseNetMapSummary: func(string) {},
		onSelfNodeChanged:      func(*netmap.NetworkMap) {},
	}
	ms.sessionAliveCtx, ms.sessionAliveCtxClose = context.WithCancel(context.Background())
	return ms
}

func (ms *mapSession) clock() tstime.Clock {
	return cmpx.Or[tstime.Clock](ms.altClock, tstime.StdClock{})
}

// StartWatchdog starts the session's watchdog timer.
// If there's no activity in too long, it tears down the connection.
// Call Close to release these resources.
func (ms *mapSession) StartWatchdog() {
	timer, timedOutChan := ms.clock().NewTimer(watchdogTimeout)
	go func() {
		defer timer.Stop()
		for {
			select {
			case <-ms.sessionAliveCtx.Done():
				ms.vlogf("netmap: ending timeout goroutine")
				return
			case <-timedOutChan:
				ms.logf("map response long-poll timed out!")
				ms.cancel()
				return
			case <-ms.watchdogReset:
				if !timer.Stop() {
					select {
					case <-timedOutChan:
					case <-ms.sessionAliveCtx.Done():
						ms.vlogf("netmap: ending timeout goroutine")
						return
					}
				}
				ms.vlogf("netmap: reset timeout timer")
				timer.Reset(watchdogTimeout)
			}
		}
	}()
}

func (ms *mapSession) Close() {
	ms.sessionAliveCtxClose()
}

// HandleNonKeepAliveMapResponse handles a non-KeepAlive MapResponse (full or
// incremental).
//
// All fields that are valid on a KeepAlive MapResponse have already been
// handled.
//
// TODO(bradfitz): make this handle all fields later. For now (2023-08-20) this
// is [re]factoring progress enough.
func (ms *mapSession) HandleNonKeepAliveMapResponse(ctx context.Context, resp *tailcfg.MapResponse) error {
	if debug := resp.Debug; debug != nil {
		if err := ms.onDebug(ctx, debug, ms.watchdogReset); err != nil {
			return err
		}
	}

	if DevKnob.StripEndpoints() {
		for _, p := range resp.Peers {
			p.Endpoints = nil
		}
		for _, p := range resp.PeersChanged {
			p.Endpoints = nil
		}
	}

	// For responses that mutate the self node, check for updated nodeAttrs.
	if resp.Node != nil {
		if DevKnob.StripCaps() {
			resp.Node.Capabilities = nil
		}
		setControlKnobsFromNodeAttrs(resp.Node.Capabilities)
	}

	// Call Node.InitDisplayNames on any changed nodes.
	initDisplayNames(cmpx.Or(resp.Node.View(), ms.lastNode), resp)

	nm := ms.netmapForResponse(resp)

	ms.lastNetmapSummary = nm.VeryConcise()
	ms.onConciseNetMapSummary(ms.lastNetmapSummary)

	// If the self node changed, we might need to update persist.
	if resp.Node != nil {
		ms.onSelfNodeChanged(nm)
	}

	ms.nu.UpdateFullNetmap(nm)
	return nil
}

func (ms *mapSession) addUserProfile(userID tailcfg.UserID) {
	if userID == 0 {
		return
	}
	nm := ms.netMapBuilding
	if _, dup := nm.UserProfiles[userID]; dup {
		// Already populated it from a previous peer.
		return
	}
	if up, ok := ms.lastUserProfile[userID]; ok {
		nm.UserProfiles[userID] = up
	}
}

// netmapForResponse returns a fully populated NetworkMap from a full
// or incremental MapResponse within the session, filling in omitted
// information from prior MapResponse values.
func (ms *mapSession) netmapForResponse(resp *tailcfg.MapResponse) *netmap.NetworkMap {
	undeltaPeers(resp, ms.previousPeers)

	ms.previousPeers = cloneNodes(resp.Peers) // defensive/lazy clone, since this escapes to who knows where
	for _, up := range resp.UserProfiles {
		ms.lastUserProfile[up.ID] = up
	}

	if dm := resp.DERPMap; dm != nil {
		ms.vlogf("netmap: new map contains DERP map")

		// Zero-valued fields in a DERPMap mean that we're not changing
		// anything and are using the previous value(s).
		if ldm := ms.lastDERPMap; ldm != nil {
			if dm.Regions == nil {
				dm.Regions = ldm.Regions
				dm.OmitDefaultRegions = ldm.OmitDefaultRegions
			}
			if dm.HomeParams == nil {
				dm.HomeParams = ldm.HomeParams
			} else if oldhh := ldm.HomeParams; oldhh != nil {
				// Propagate sub-fields of HomeParams
				hh := dm.HomeParams
				if hh.RegionScore == nil {
					hh.RegionScore = oldhh.RegionScore
				}
			}
		}

		ms.lastDERPMap = dm
	}

	if pf := resp.PacketFilter; pf != nil {
		var err error
		ms.lastPacketFilterRules = views.SliceOf(pf)
		ms.lastParsedPacketFilter, err = filter.MatchesFromFilterRules(pf)
		if err != nil {
			ms.logf("parsePacketFilter: %v", err)
		}
	}
	if c := resp.DNSConfig; c != nil {
		ms.lastDNSConfig = c
	}
	if p := resp.SSHPolicy; p != nil {
		ms.lastSSHPolicy = p
	}

	if v, ok := resp.CollectServices.Get(); ok {
		ms.collectServices = v
	}
	if resp.Domain != "" {
		ms.lastDomain = resp.Domain
	}
	if resp.DomainDataPlaneAuditLogID != "" {
		ms.lastDomainAuditLogID = resp.DomainDataPlaneAuditLogID
	}
	if resp.Health != nil {
		ms.lastHealth = resp.Health
	}
	if resp.TKAInfo != nil {
		ms.lastTKAInfo = resp.TKAInfo
	}

	// TODO(bradfitz): now that this is a view, remove some of the defensive
	// cloning elsewhere in mapSession.
	peerViews := make([]tailcfg.NodeView, len(resp.Peers))
	for i, n := range resp.Peers {
		peerViews[i] = n.View()
	}

	nm := &netmap.NetworkMap{
		NodeKey:           ms.privateNodeKey.Public(),
		PrivateKey:        ms.privateNodeKey,
		MachineKey:        ms.machinePubKey,
		Peers:             peerViews,
		UserProfiles:      make(map[tailcfg.UserID]tailcfg.UserProfile),
		Domain:            ms.lastDomain,
		DomainAuditLogID:  ms.lastDomainAuditLogID,
		DNS:               *ms.lastDNSConfig,
		PacketFilter:      ms.lastParsedPacketFilter,
		PacketFilterRules: ms.lastPacketFilterRules,
		SSHPolicy:         ms.lastSSHPolicy,
		CollectServices:   ms.collectServices,
		DERPMap:           ms.lastDERPMap,
		ControlHealth:     ms.lastHealth,
		TKAEnabled:        ms.lastTKAInfo != nil && !ms.lastTKAInfo.Disabled,
	}
	ms.netMapBuilding = nm

	if ms.lastTKAInfo != nil && ms.lastTKAInfo.Head != "" {
		if err := nm.TKAHead.UnmarshalText([]byte(ms.lastTKAInfo.Head)); err != nil {
			ms.logf("error unmarshalling TKAHead: %v", err)
			nm.TKAEnabled = false
		}
	}

	if resp.Node != nil {
		ms.lastNode = resp.Node.View()
	}
	if node := ms.lastNode; node.Valid() {
		nm.SelfNode = node
		nm.Expiry = node.KeyExpiry()
		nm.Name = node.Name()
		nm.Addresses = filterSelfAddresses(node.Addresses().AsSlice())
		if node.Hostinfo().Valid() {
			nm.Hostinfo = *node.Hostinfo().AsStruct()
		}
		if node.MachineAuthorized() {
			nm.MachineStatus = tailcfg.MachineAuthorized
		} else {
			nm.MachineStatus = tailcfg.MachineUnauthorized
		}
	}

	ms.addUserProfile(nm.User())
	for _, peer := range resp.Peers {
		ms.addUserProfile(peer.Sharer)
		ms.addUserProfile(peer.User)
	}
	if DevKnob.ForceProxyDNS() {
		nm.DNS.Proxied = true
	}
	ms.netMapBuilding = nil
	return nm
}

// undeltaPeers updates mapRes.Peers to be complete based on the
// provided previous peer list and the PeersRemoved and PeersChanged
// fields in mapRes, as well as the PeerSeenChange and OnlineChange
// maps.
//
// It then also nils out the delta fields.
func undeltaPeers(mapRes *tailcfg.MapResponse, prev []*tailcfg.Node) {
	if len(mapRes.Peers) > 0 {
		// Not delta encoded.
		if !nodesSorted(mapRes.Peers) {
			log.Printf("netmap: undeltaPeers: MapResponse.Peers not sorted; sorting")
			sortNodes(mapRes.Peers)
		}
		return
	}

	var removed map[tailcfg.NodeID]bool
	if pr := mapRes.PeersRemoved; len(pr) > 0 {
		removed = make(map[tailcfg.NodeID]bool, len(pr))
		for _, id := range pr {
			removed[id] = true
		}
	}
	changed := mapRes.PeersChanged

	if !nodesSorted(changed) {
		log.Printf("netmap: undeltaPeers: MapResponse.PeersChanged not sorted; sorting")
		sortNodes(changed)
	}
	if !nodesSorted(prev) {
		// Internal error (unrelated to the network) if we get here.
		log.Printf("netmap: undeltaPeers: [unexpected] prev not sorted; sorting")
		sortNodes(prev)
	}

	newFull := prev
	if len(removed) > 0 || len(changed) > 0 {
		newFull = make([]*tailcfg.Node, 0, len(prev)-len(removed))
		for len(prev) > 0 && len(changed) > 0 {
			pID := prev[0].ID
			cID := changed[0].ID
			if removed[pID] {
				prev = prev[1:]
				continue
			}
			switch {
			case pID < cID:
				newFull = append(newFull, prev[0])
				prev = prev[1:]
			case pID == cID:
				newFull = append(newFull, changed[0])
				prev, changed = prev[1:], changed[1:]
			case cID < pID:
				newFull = append(newFull, changed[0])
				changed = changed[1:]
			}
		}
		newFull = append(newFull, changed...)
		for _, n := range prev {
			if !removed[n.ID] {
				newFull = append(newFull, n)
			}
		}
		sortNodes(newFull)
	}

	if len(mapRes.PeerSeenChange) != 0 || len(mapRes.OnlineChange) != 0 || len(mapRes.PeersChangedPatch) != 0 {
		peerByID := make(map[tailcfg.NodeID]*tailcfg.Node, len(newFull))
		for _, n := range newFull {
			peerByID[n.ID] = n
		}
		now := clock.Now()
		for nodeID, seen := range mapRes.PeerSeenChange {
			if n, ok := peerByID[nodeID]; ok {
				if seen {
					n.LastSeen = &now
				} else {
					n.LastSeen = nil
				}
			}
		}
		for nodeID, online := range mapRes.OnlineChange {
			if n, ok := peerByID[nodeID]; ok {
				online := online
				n.Online = &online
			}
		}
		for _, ec := range mapRes.PeersChangedPatch {
			if n, ok := peerByID[ec.NodeID]; ok {
				if ec.DERPRegion != 0 {
					n.DERP = fmt.Sprintf("%s:%v", tailcfg.DerpMagicIP, ec.DERPRegion)
				}
				if ec.Cap != 0 {
					n.Cap = ec.Cap
				}
				if ec.Endpoints != nil {
					n.Endpoints = ec.Endpoints
				}
				if ec.Key != nil {
					n.Key = *ec.Key
				}
				if ec.DiscoKey != nil {
					n.DiscoKey = *ec.DiscoKey
				}
				if v := ec.Online; v != nil {
					n.Online = ptrCopy(v)
				}
				if v := ec.LastSeen; v != nil {
					n.LastSeen = ptrCopy(v)
				}
				if v := ec.KeyExpiry; v != nil {
					n.KeyExpiry = *v
				}
				if v := ec.Capabilities; v != nil {
					n.Capabilities = *v
				}
				if v := ec.KeySignature; v != nil {
					n.KeySignature = v
				}
			}
		}
	}

	mapRes.Peers = newFull
	mapRes.PeersChanged = nil
	mapRes.PeersRemoved = nil
}

// ptrCopy returns a pointer to a newly allocated shallow copy of *v.
func ptrCopy[T any](v *T) *T {
	if v == nil {
		return nil
	}
	ret := new(T)
	*ret = *v
	return ret
}

func nodesSorted(v []*tailcfg.Node) bool {
	for i, n := range v {
		if i > 0 && n.ID <= v[i-1].ID {
			return false
		}
	}
	return true
}

func sortNodes(v []*tailcfg.Node) {
	sort.Slice(v, func(i, j int) bool { return v[i].ID < v[j].ID })
}

func cloneNodes(v1 []*tailcfg.Node) []*tailcfg.Node {
	if v1 == nil {
		return nil
	}
	v2 := make([]*tailcfg.Node, len(v1))
	for i, n := range v1 {
		v2[i] = n.Clone()
	}
	return v2
}

var debugSelfIPv6Only = envknob.RegisterBool("TS_DEBUG_SELF_V6_ONLY")

func filterSelfAddresses(in []netip.Prefix) (ret []netip.Prefix) {
	switch {
	default:
		return in
	case debugSelfIPv6Only():
		for _, a := range in {
			if a.Addr().Is6() {
				ret = append(ret, a)
			}
		}
		return ret
	}
}
