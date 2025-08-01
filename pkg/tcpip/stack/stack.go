// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package stack provides the glue between networking protocols and the
// consumers of the networking stack.
//
// For consumers, the only function of interest is New(), everything else is
// provided by the tcpip/public package.
package stack

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"time"

	"golang.org/x/time/rate"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/log"
	cryptorand "gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/ports"
	"gvisor.dev/gvisor/pkg/waiter"
)

const (
	// DefaultTOS is the default type of service value for network endpoints.
	DefaultTOS = 0
)

// +stateify savable
type transportProtocolState struct {
	proto          TransportProtocol
	defaultHandler func(id TransportEndpointID, pkt *PacketBuffer) bool `state:"nosave"`
}

// RestoredEndpoint is an endpoint that needs to be restored.
type RestoredEndpoint interface {
	// Restore restores an endpoint. This can be used to restart background
	// workers such as protocol goroutines. This must be called after all
	// indirect dependencies of the endpoint has been restored, which
	// generally implies at the end of the restore process.
	Restore(*Stack)
}

// ResumableEndpoint is an endpoint that needs to be resumed after save.
type ResumableEndpoint interface {
	// Resume resumes an endpoint.
	Resume()
}

var netRawMissingLogger = log.BasicRateLimitedLogger(time.Minute)

// Stack is a networking stack, with all supported protocols, NICs, and route
// table.
//
// LOCK ORDERING: mu > routeMu.
//
// +stateify savable
type Stack struct {
	transportProtocols map[tcpip.TransportProtocolNumber]*transportProtocolState
	networkProtocols   map[tcpip.NetworkProtocolNumber]NetworkProtocol

	// rawFactory creates raw endpoints. If nil, raw endpoints are
	// disabled. It is set during Stack creation and is immutable.
	rawFactory                   RawFactory
	packetEndpointWriteSupported bool

	demux *transportDemuxer

	stats tcpip.Stats

	// routeMu protects annotated fields below.
	routeMu routeStackRWMutex `state:"nosave"`

	// routeTable is a list of routes sorted by prefix length, longest (most specific) first.
	// +checklocks:routeMu
	routeTable tcpip.RouteList `state:"nosave"`

	mu stackRWMutex `state:"nosave"`
	// +checklocks:mu
	nics map[tcpip.NICID]*nic `state:"nosave"`
	// +checklocks:mu
	defaultForwardingEnabled map[tcpip.NetworkProtocolNumber]struct{}

	// nicIDGen is used to generate NIC IDs.
	nicIDGen atomicbitops.Int32 `state:"nosave"`

	// cleanupEndpointsMu protects cleanupEndpoints.
	cleanupEndpointsMu cleanupEndpointsMutex `state:"nosave"`
	// +checklocks:cleanupEndpointsMu
	cleanupEndpoints map[TransportEndpoint]struct{}

	*ports.PortManager

	// clock is used to generate user-visible times.
	clock tcpip.Clock

	// handleLocal allows non-loopback interfaces to loop packets.
	handleLocal bool

	// tables are the iptables packet filtering and manipulation rules.
	// TODO(gvisor.dev/issue/4595): S/R this field.
	tables *IPTables `state:"nosave"`

	// nftables is the nftables interface for packet filtering and manipulation rules.
	nftables NFTablesInterface `state:"nosave"`

	// restoredEndpoints is a list of endpoints that need to be restored if the
	// stack is being restored.
	restoredEndpoints []RestoredEndpoint

	// resumableEndpoints is a list of endpoints that need to be resumed
	// after save.
	resumableEndpoints []ResumableEndpoint

	// icmpRateLimiter is a global rate limiter for all ICMP messages generated
	// by the stack.
	icmpRateLimiter *ICMPRateLimiter

	// seed is a one-time random value initialized at stack startup.
	//
	// TODO(gvisor.dev/issue/940): S/R this field.
	seed uint32

	// nudConfigs is the default NUD configurations used by interfaces.
	nudConfigs NUDConfigurations

	// nudDisp is the NUD event dispatcher that is used to send the netstack
	// integrator NUD related events.
	nudDisp NUDDispatcher

	// randomGenerator is an injectable pseudo random generator that can be
	// used when a random number is required. It must not be used in
	// security-sensitive contexts.
	insecureRNG *rand.Rand `state:"nosave"`

	// secureRNG is a cryptographically secure random number generator.
	secureRNG cryptorand.RNG `state:"nosave"`

	// sendBufferSize holds the min/default/max send buffer sizes for
	// endpoints other than TCP.
	sendBufferSize tcpip.SendBufferSizeOption

	// receiveBufferSize holds the min/default/max receive buffer sizes for
	// endpoints other than TCP.
	receiveBufferSize tcpip.ReceiveBufferSizeOption

	// tcpInvalidRateLimit is the maximal rate for sending duplicate
	// acknowledgements in response to incoming TCP packets that are for an existing
	// connection but that are invalid due to any of the following reasons:
	//
	//   a) out-of-window sequence number.
	//   b) out-of-window acknowledgement number.
	//   c) PAWS check failure (when implemented).
	//
	// This is required to prevent potential ACK loops.
	// Setting this to 0 will disable all rate limiting.
	tcpInvalidRateLimit time.Duration

	// tsOffsetSecret is the secret key for generating timestamp offsets
	// initialized at stack startup.
	tsOffsetSecret uint32

	// saveRestoreEnabled indicates whether the stack is saved and restored.
	saveRestoreEnabled bool
}

// NetworkProtocolFactory instantiates a network protocol.
//
// NetworkProtocolFactory must not attempt to modify the stack, it may only
// query the stack.
type NetworkProtocolFactory func(*Stack) NetworkProtocol

// TransportProtocolFactory instantiates a transport protocol.
//
// TransportProtocolFactory must not attempt to modify the stack, it may only
// query the stack.
type TransportProtocolFactory func(*Stack) TransportProtocol

// Options contains optional Stack configuration.
type Options struct {
	// NetworkProtocols lists the network protocols to enable.
	NetworkProtocols []NetworkProtocolFactory

	// TransportProtocols lists the transport protocols to enable.
	TransportProtocols []TransportProtocolFactory

	// Clock is an optional clock used for timekeeping.
	//
	// If Clock is nil, tcpip.NewStdClock() will be used.
	Clock tcpip.Clock

	// Stats are optional statistic counters.
	Stats tcpip.Stats

	// HandleLocal indicates whether packets destined to their source
	// should be handled by the stack internally (true) or outside the
	// stack (false).
	HandleLocal bool

	// NUDConfigs is the default NUD configurations used by interfaces.
	NUDConfigs NUDConfigurations

	// NUDDisp is the NUD event dispatcher that an integrator can provide to
	// receive NUD related events.
	NUDDisp NUDDispatcher

	// RawFactory produces raw endpoints. Raw endpoints are enabled only if
	// this is non-nil.
	RawFactory RawFactory

	// AllowPacketEndpointWrite determines if packet endpoints support write
	// operations.
	AllowPacketEndpointWrite bool

	// RandSource is an optional source to use to generate random
	// numbers. If omitted it defaults to a Source seeded by the data
	// returned by the stack secure RNG.
	//
	// RandSource must be thread-safe.
	RandSource rand.Source

	// IPTables are the initial iptables rules. If nil, DefaultIPTables will be
	// used to construct the initial iptables rules.
	// all traffic.
	IPTables *IPTables

	// NFTables is the nftables interface for packet filtering and manipulation rules.
	NFTables NFTablesInterface

	// DefaultIPTables is an optional iptables rules constructor that is called
	// if IPTables is nil. If both fields are nil, iptables will allow all
	// traffic.
	DefaultIPTables func(clock tcpip.Clock, rand *rand.Rand) *IPTables

	// SecureRNG is a cryptographically secure random number generator.
	SecureRNG io.Reader
}

// TransportEndpointInfo holds useful information about a transport endpoint
// which can be queried by monitoring tools.
//
// +stateify savable
type TransportEndpointInfo struct {
	// The following fields are initialized at creation time and are
	// immutable.

	NetProto   tcpip.NetworkProtocolNumber
	TransProto tcpip.TransportProtocolNumber

	// The following fields are protected by endpoint mu.

	ID TransportEndpointID
	// BindNICID and bindAddr are set via calls to Bind(). They are used to
	// reject attempts to send data or connect via a different NIC or
	// address
	BindNICID tcpip.NICID
	BindAddr  tcpip.Address
	// RegisterNICID is the default NICID registered as a side-effect of
	// connect or datagram write.
	RegisterNICID tcpip.NICID
}

// AddrNetProtoLocked unwraps the specified address if it is a V4-mapped V6
// address and returns the network protocol number to be used to communicate
// with the specified address. It returns an error if the passed address is
// incompatible with the receiver.
//
// Preconditon: the parent endpoint mu must be held while calling this method.
func (t *TransportEndpointInfo) AddrNetProtoLocked(addr tcpip.FullAddress, v6only bool, bind bool) (tcpip.FullAddress, tcpip.NetworkProtocolNumber, tcpip.Error) {
	netProto := t.NetProto
	switch addr.Addr.BitLen() {
	case header.IPv4AddressSizeBits:
		netProto = header.IPv4ProtocolNumber
	case header.IPv6AddressSizeBits:
		if header.IsV4MappedAddress(addr.Addr) {
			netProto = header.IPv4ProtocolNumber
			addr.Addr = tcpip.AddrFrom4Slice(addr.Addr.AsSlice()[header.IPv6AddressSize-header.IPv4AddressSize:])
			if addr.Addr == header.IPv4Any {
				addr.Addr = tcpip.Address{}
			}
		}
	}

	switch t.ID.LocalAddress.BitLen() {
	case header.IPv4AddressSizeBits:
		if addr.Addr.BitLen() == header.IPv6AddressSizeBits {
			return tcpip.FullAddress{}, 0, &tcpip.ErrInvalidEndpointState{}
		}
	case header.IPv6AddressSizeBits:
		if addr.Addr.BitLen() == header.IPv4AddressSizeBits {
			return tcpip.FullAddress{}, 0, &tcpip.ErrNetworkUnreachable{}
		}
	}

	if !bind && addr.Addr.Unspecified() {
		// If the destination address isn't set, Linux sets it to the
		// source address. If a source address isn't set either, it
		// sets both to the loopback address.
		if t.ID.LocalAddress.Unspecified() {
			switch netProto {
			case header.IPv4ProtocolNumber:
				addr.Addr = header.IPv4Loopback
			case header.IPv6ProtocolNumber:
				addr.Addr = header.IPv6Loopback
			}
		} else {
			addr.Addr = t.ID.LocalAddress
		}
	}

	switch {
	case netProto == t.NetProto:
	case netProto == header.IPv4ProtocolNumber && t.NetProto == header.IPv6ProtocolNumber:
		if v6only {
			return tcpip.FullAddress{}, 0, &tcpip.ErrHostUnreachable{}
		}
	default:
		return tcpip.FullAddress{}, 0, &tcpip.ErrInvalidEndpointState{}
	}

	return addr, netProto, nil
}

// IsEndpointInfo is an empty method to implement the tcpip.EndpointInfo
// marker interface.
func (*TransportEndpointInfo) IsEndpointInfo() {}

// New allocates a new networking stack with only the requested networking and
// transport protocols configured with default options.
//
// Note, NDPConfigurations will be fixed before being used by the Stack. That
// is, if an invalid value was provided, it will be reset to the default value.
//
// Protocol options can be changed by calling the
// SetNetworkProtocolOption/SetTransportProtocolOption methods provided by the
// stack. Please refer to individual protocol implementations as to what options
// are supported.
func New(opts Options) *Stack {
	clock := opts.Clock
	if clock == nil {
		clock = tcpip.NewStdClock()
	}

	if opts.SecureRNG == nil {
		opts.SecureRNG = cryptorand.Reader
	}
	secureRNG := cryptorand.RNGFrom(opts.SecureRNG)

	randSrc := opts.RandSource
	if randSrc == nil {
		var v int64
		if err := binary.Read(opts.SecureRNG, binary.LittleEndian, &v); err != nil {
			panic(err)
		}
		// Source provided by rand.NewSource is not thread-safe so
		// we wrap it in a simple thread-safe version.
		randSrc = &lockedRandomSource{src: rand.NewSource(v)}
	}
	insecureRNG := rand.New(randSrc)

	if opts.IPTables == nil {
		if opts.DefaultIPTables == nil {
			opts.DefaultIPTables = DefaultTables
		}
		opts.IPTables = opts.DefaultIPTables(clock, insecureRNG)
	}

	opts.NUDConfigs.resetInvalidFields()

	s := &Stack{
		transportProtocols:           make(map[tcpip.TransportProtocolNumber]*transportProtocolState),
		networkProtocols:             make(map[tcpip.NetworkProtocolNumber]NetworkProtocol),
		nics:                         make(map[tcpip.NICID]*nic),
		packetEndpointWriteSupported: opts.AllowPacketEndpointWrite,
		defaultForwardingEnabled:     make(map[tcpip.NetworkProtocolNumber]struct{}),
		cleanupEndpoints:             make(map[TransportEndpoint]struct{}),
		PortManager:                  ports.NewPortManager(),
		clock:                        clock,
		stats:                        opts.Stats.FillIn(),
		handleLocal:                  opts.HandleLocal,
		tables:                       opts.IPTables,
		nftables:                     opts.NFTables,
		icmpRateLimiter:              NewICMPRateLimiter(clock),
		seed:                         secureRNG.Uint32(),
		nudConfigs:                   opts.NUDConfigs,
		nudDisp:                      opts.NUDDisp,
		insecureRNG:                  insecureRNG,
		secureRNG:                    secureRNG,
		sendBufferSize: tcpip.SendBufferSizeOption{
			Min:     MinBufferSize,
			Default: DefaultBufferSize,
			Max:     DefaultMaxBufferSize,
		},
		receiveBufferSize: tcpip.ReceiveBufferSizeOption{
			Min:     MinBufferSize,
			Default: DefaultBufferSize,
			Max:     DefaultMaxBufferSize,
		},
		tcpInvalidRateLimit: defaultTCPInvalidRateLimit,
		tsOffsetSecret:      secureRNG.Uint32(),
	}

	// Add specified network protocols.
	for _, netProtoFactory := range opts.NetworkProtocols {
		netProto := netProtoFactory(s)
		s.networkProtocols[netProto.Number()] = netProto
	}

	// Add specified transport protocols.
	for _, transProtoFactory := range opts.TransportProtocols {
		transProto := transProtoFactory(s)
		s.transportProtocols[transProto.Number()] = &transportProtocolState{
			proto: transProto,
		}
	}

	// Add the factory for raw endpoints, if present.
	s.rawFactory = opts.RawFactory

	// Create the global transport demuxer.
	s.demux = newTransportDemuxer(s)

	return s
}

// NextNICID allocates the next available NIC ID and returns it.
func (s *Stack) NextNICID() tcpip.NICID {
	next := s.nicIDGen.Add(1)
	if next < 0 {
		panic("NICID overflow")
	}
	return tcpip.NICID(next)
}

// SetNetworkProtocolOption allows configuring individual protocol level
// options. This method returns an error if the protocol is not supported or
// option is not supported by the protocol implementation or the provided value
// is incorrect.
func (s *Stack) SetNetworkProtocolOption(network tcpip.NetworkProtocolNumber, option tcpip.SettableNetworkProtocolOption) tcpip.Error {
	netProto, ok := s.networkProtocols[network]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}
	return netProto.SetOption(option)
}

// NetworkProtocolOption allows retrieving individual protocol level option
// values. This method returns an error if the protocol is not supported or
// option is not supported by the protocol implementation. E.g.:
//
//	var v ipv4.MyOption
//	err := s.NetworkProtocolOption(tcpip.IPv4ProtocolNumber, &v)
//	if err != nil {
//		...
//	}
func (s *Stack) NetworkProtocolOption(network tcpip.NetworkProtocolNumber, option tcpip.GettableNetworkProtocolOption) tcpip.Error {
	netProto, ok := s.networkProtocols[network]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}
	return netProto.Option(option)
}

// SetTransportProtocolOption allows configuring individual protocol level
// options. This method returns an error if the protocol is not supported or
// option is not supported by the protocol implementation or the provided value
// is incorrect.
func (s *Stack) SetTransportProtocolOption(transport tcpip.TransportProtocolNumber, option tcpip.SettableTransportProtocolOption) tcpip.Error {
	transProtoState, ok := s.transportProtocols[transport]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}
	return transProtoState.proto.SetOption(option)
}

// TransportProtocolOption allows retrieving individual protocol level option
// values. This method returns an error if the protocol is not supported or
// option is not supported by the protocol implementation.
//
//	var v tcp.SACKEnabled
//	if err := s.TransportProtocolOption(tcpip.TCPProtocolNumber, &v); err != nil {
//		...
//	}
func (s *Stack) TransportProtocolOption(transport tcpip.TransportProtocolNumber, option tcpip.GettableTransportProtocolOption) tcpip.Error {
	transProtoState, ok := s.transportProtocols[transport]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}
	return transProtoState.proto.Option(option)
}

// SendBufSizeProto is a protocol that can return its send buffer size.
type SendBufSizeProto interface {
	SendBufferSize() tcpip.TCPSendBufferSizeRangeOption
}

// TCPSendBufferLimits returns the TCP send buffer size limit.
func (s *Stack) TCPSendBufferLimits() tcpip.TCPSendBufferSizeRangeOption {
	return s.transportProtocols[header.TCPProtocolNumber].proto.(SendBufSizeProto).SendBufferSize()
}

// SetTransportProtocolHandler sets the per-stack default handler for the given
// protocol.
//
// It must be called only during initialization of the stack. Changing it as the
// stack is operating is not supported.
func (s *Stack) SetTransportProtocolHandler(p tcpip.TransportProtocolNumber, h func(TransportEndpointID, *PacketBuffer) bool) {
	state := s.transportProtocols[p]
	if state != nil {
		state.defaultHandler = h
	}
}

// Clock returns the Stack's clock for retrieving the current time and
// scheduling work.
func (s *Stack) Clock() tcpip.Clock {
	return s.clock
}

// Stats returns a mutable copy of the current stats.
//
// This is not generally exported via the public interface, but is available
// internally.
func (s *Stack) Stats() tcpip.Stats {
	return s.stats
}

// SetNICForwarding enables or disables packet forwarding on the specified NIC
// for the passed protocol.
//
// Returns the previous configuration on the NIC.
func (s *Stack) SetNICForwarding(id tcpip.NICID, protocol tcpip.NetworkProtocolNumber, enable bool) (bool, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return false, &tcpip.ErrUnknownNICID{}
	}

	return nic.setForwarding(protocol, enable)
}

// NICForwarding returns the forwarding configuration for the specified NIC.
func (s *Stack) NICForwarding(id tcpip.NICID, protocol tcpip.NetworkProtocolNumber) (bool, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return false, &tcpip.ErrUnknownNICID{}
	}

	return nic.forwarding(protocol)
}

// SetForwardingDefaultAndAllNICs sets packet forwarding for all NICs for the
// passed protocol and sets the default setting for newly created NICs.
func (s *Stack) SetForwardingDefaultAndAllNICs(protocol tcpip.NetworkProtocolNumber, enable bool) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	doneOnce := false
	for id, nic := range s.nics {
		if _, err := nic.setForwarding(protocol, enable); err != nil {
			// Expect forwarding to be settable on all interfaces if it was set on
			// one.
			if doneOnce {
				panic(fmt.Sprintf("nic(id=%d).setForwarding(%d, %t): %s", id, protocol, enable, err))
			}

			return err
		}

		doneOnce = true
	}

	if enable {
		s.defaultForwardingEnabled[protocol] = struct{}{}
	} else {
		delete(s.defaultForwardingEnabled, protocol)
	}

	return nil
}

// AddMulticastRoute adds a multicast route to be used for the specified
// addresses and protocol.
func (s *Stack) AddMulticastRoute(protocol tcpip.NetworkProtocolNumber, addresses UnicastSourceAndMulticastDestination, route MulticastRoute) tcpip.Error {
	netProto, ok := s.networkProtocols[protocol]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}

	forwardingNetProto, ok := netProto.(MulticastForwardingNetworkProtocol)
	if !ok {
		return &tcpip.ErrNotSupported{}
	}

	return forwardingNetProto.AddMulticastRoute(addresses, route)
}

// RemoveMulticastRoute removes a multicast route that matches the specified
// addresses and protocol.
func (s *Stack) RemoveMulticastRoute(protocol tcpip.NetworkProtocolNumber, addresses UnicastSourceAndMulticastDestination) tcpip.Error {
	netProto, ok := s.networkProtocols[protocol]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}

	forwardingNetProto, ok := netProto.(MulticastForwardingNetworkProtocol)
	if !ok {
		return &tcpip.ErrNotSupported{}
	}

	return forwardingNetProto.RemoveMulticastRoute(addresses)
}

// MulticastRouteLastUsedTime returns a monotonic timestamp that represents the
// last time that the route that matches the provided addresses and protocol
// was used or updated.
func (s *Stack) MulticastRouteLastUsedTime(protocol tcpip.NetworkProtocolNumber, addresses UnicastSourceAndMulticastDestination) (tcpip.MonotonicTime, tcpip.Error) {
	netProto, ok := s.networkProtocols[protocol]
	if !ok {
		return tcpip.MonotonicTime{}, &tcpip.ErrUnknownProtocol{}
	}

	forwardingNetProto, ok := netProto.(MulticastForwardingNetworkProtocol)
	if !ok {
		return tcpip.MonotonicTime{}, &tcpip.ErrNotSupported{}
	}

	return forwardingNetProto.MulticastRouteLastUsedTime(addresses)
}

// EnableMulticastForwardingForProtocol enables multicast forwarding for the
// provided protocol.
//
// Returns true if forwarding was already enabled on the protocol.
// Additionally, returns an error if:
//
//   - The protocol is not found.
//   - The protocol doesn't support multicast forwarding.
//   - The multicast forwarding event dispatcher is nil.
//
// If successful, future multicast forwarding events will be sent to the
// provided event dispatcher.
func (s *Stack) EnableMulticastForwardingForProtocol(protocol tcpip.NetworkProtocolNumber, disp MulticastForwardingEventDispatcher) (bool, tcpip.Error) {
	netProto, ok := s.networkProtocols[protocol]
	if !ok {
		return false, &tcpip.ErrUnknownProtocol{}
	}

	forwardingNetProto, ok := netProto.(MulticastForwardingNetworkProtocol)
	if !ok {
		return false, &tcpip.ErrNotSupported{}
	}

	return forwardingNetProto.EnableMulticastForwarding(disp)
}

// DisableMulticastForwardingForProtocol disables multicast forwarding for the
// provided protocol.
//
// Returns an error if the provided protocol is not found or if it does not
// support multicast forwarding.
func (s *Stack) DisableMulticastForwardingForProtocol(protocol tcpip.NetworkProtocolNumber) tcpip.Error {
	netProto, ok := s.networkProtocols[protocol]
	if !ok {
		return &tcpip.ErrUnknownProtocol{}
	}

	forwardingNetProto, ok := netProto.(MulticastForwardingNetworkProtocol)
	if !ok {
		return &tcpip.ErrNotSupported{}
	}

	forwardingNetProto.DisableMulticastForwarding()
	return nil
}

// SetNICMulticastForwarding enables or disables multicast packet forwarding on
// the specified NIC for the passed protocol.
//
// Returns the previous configuration on the NIC.
func (s *Stack) SetNICMulticastForwarding(id tcpip.NICID, protocol tcpip.NetworkProtocolNumber, enable bool) (bool, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return false, &tcpip.ErrUnknownNICID{}
	}

	return nic.setMulticastForwarding(protocol, enable)
}

// NICMulticastForwarding returns the multicast forwarding configuration for
// the specified NIC.
func (s *Stack) NICMulticastForwarding(id tcpip.NICID, protocol tcpip.NetworkProtocolNumber) (bool, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return false, &tcpip.ErrUnknownNICID{}
	}

	return nic.multicastForwarding(protocol)
}

// PortRange returns the UDP and TCP inclusive range of ephemeral ports used in
// both IPv4 and IPv6.
func (s *Stack) PortRange() (uint16, uint16) {
	return s.PortManager.PortRange()
}

// SetPortRange sets the UDP and TCP IPv4 and IPv6 ephemeral port range
// (inclusive).
func (s *Stack) SetPortRange(start uint16, end uint16) tcpip.Error {
	return s.PortManager.SetPortRange(start, end)
}

// SetRouteTable assigns the route table to be used by this stack. It
// specifies which NIC to use for given destination address ranges.
//
// This method takes ownership of the table.
func (s *Stack) SetRouteTable(table []tcpip.Route) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.routeTable.Reset()
	for _, r := range table {
		s.addRouteLocked(&r)
	}
}

// GetRouteTable returns the route table which is currently in use.
func (s *Stack) GetRouteTable() []tcpip.Route {
	s.routeMu.RLock()
	defer s.routeMu.RUnlock()
	table := make([]tcpip.Route, 0)
	for r := s.routeTable.Front(); r != nil; r = r.Next() {
		table = append(table, *r)
	}
	return table
}

// AddRoute appends a route to the route table.
func (s *Stack) AddRoute(route tcpip.Route) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.addRouteLocked(&route)
}

// +checklocks:s.routeMu
func (s *Stack) addRouteLocked(route *tcpip.Route) {
	routePrefix := route.Destination.Prefix()
	n := s.routeTable.Front()
	for ; n != nil; n = n.Next() {
		if n.Destination.Prefix() < routePrefix {
			s.routeTable.InsertBefore(n, route)
			return
		}
	}
	s.routeTable.PushBack(route)
}

// RemoveRoutes removes matching routes from the route table, it
// returns the number of routes that are removed.
func (s *Stack) RemoveRoutes(match func(tcpip.Route) bool) int {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	return s.removeRoutesLocked(match)
}

// +checklocks:s.routeMu
func (s *Stack) removeRoutesLocked(match func(tcpip.Route) bool) int {
	count := 0
	for route := s.routeTable.Front(); route != nil; {
		next := route.Next()
		if match(*route) {
			s.routeTable.Remove(route)
			count++
		}
		route = next
	}
	return count
}

// ReplaceRoute replaces the route in the routing table which matchse
// the lookup key for the routing table. If there is no match, the given
// route will still be added to the routing table.
// The lookup key consists of destination, ToS, scope and output interface.
func (s *Stack) ReplaceRoute(route tcpip.Route) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()

	s.removeRoutesLocked(func(rt tcpip.Route) bool {
		return rt.Equal(route)
	})
	s.addRouteLocked(&route)
}

// NewEndpoint creates a new transport layer endpoint of the given protocol.
func (s *Stack) NewEndpoint(transport tcpip.TransportProtocolNumber, network tcpip.NetworkProtocolNumber, waiterQueue *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	t, ok := s.transportProtocols[transport]
	if !ok {
		return nil, &tcpip.ErrUnknownProtocol{}
	}

	return t.proto.NewEndpoint(network, waiterQueue)
}

// NewRawEndpoint creates a new raw transport layer endpoint of the given
// protocol. Raw endpoints receive all traffic for a given protocol regardless
// of address.
func (s *Stack) NewRawEndpoint(transport tcpip.TransportProtocolNumber, network tcpip.NetworkProtocolNumber, waiterQueue *waiter.Queue, associated bool) (tcpip.Endpoint, tcpip.Error) {
	if s.rawFactory == nil {
		netRawMissingLogger.Infof("A process tried to create a raw socket, but --net-raw was not specified. Should runsc be run with --net-raw?")
		return nil, &tcpip.ErrNotPermitted{}
	}

	if !associated {
		return s.rawFactory.NewUnassociatedEndpoint(s, network, transport, waiterQueue)
	}

	t, ok := s.transportProtocols[transport]
	if !ok {
		return nil, &tcpip.ErrUnknownProtocol{}
	}

	return t.proto.NewRawEndpoint(network, waiterQueue)
}

// NewPacketEndpoint creates a new packet endpoint listening for the given
// netProto.
func (s *Stack) NewPacketEndpoint(cooked bool, netProto tcpip.NetworkProtocolNumber, waiterQueue *waiter.Queue) (tcpip.Endpoint, tcpip.Error) {
	if s.rawFactory == nil {
		return nil, &tcpip.ErrNotPermitted{}
	}

	return s.rawFactory.NewPacketEndpoint(s, cooked, netProto, waiterQueue)
}

// NICContext is an opaque pointer used to store client-supplied NIC metadata.
type NICContext any

// NICOptions specifies the configuration of a NIC as it is being created.
// The zero value creates an enabled, unnamed NIC.
type NICOptions struct {
	// Name specifies the name of the NIC.
	Name string

	// Disabled specifies whether to avoid calling Attach on the passed
	// LinkEndpoint.
	Disabled bool

	// Context specifies user-defined data that will be returned in stack.NICInfo
	// for the NIC. Clients of this library can use it to add metadata that
	// should be tracked alongside a NIC, to avoid having to keep a
	// map[tcpip.NICID]metadata mirroring stack.Stack's nic map.
	Context NICContext

	// QDisc is the queue discipline to use for this NIC.
	QDisc QueueingDiscipline

	// DeliverLinkPackets specifies whether the NIC is responsible for
	// delivering raw packets to packet sockets.
	DeliverLinkPackets bool

	// EnableExperimentIPOption specifies whether the NIC is responsible for
	// passing the experiment IP option.
	EnableExperimentIPOption bool
}

// GetNICByID return a network device associated with the specified ID.
func (s *Stack) GetNICByID(id tcpip.NICID) (*nic, tcpip.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.nics[id]
	if !ok {
		return nil, &tcpip.ErrNoSuchFile{}
	}
	return n, nil
}

// CreateNICWithOptions creates a NIC with the provided id, LinkEndpoint, and
// NICOptions. See the documentation on type NICOptions for details on how
// NICs can be configured.
//
// LinkEndpoint.Attach will be called to bind ep with a NetworkDispatcher.
func (s *Stack) CreateNICWithOptions(id tcpip.NICID, ep LinkEndpoint, opts NICOptions) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id == 0 {
		return &tcpip.ErrInvalidNICID{}
	}
	// Make sure id is unique.
	if _, ok := s.nics[id]; ok {
		return &tcpip.ErrDuplicateNICID{}
	}

	// Make sure name is unique, unless unnamed.
	if opts.Name != "" {
		for _, n := range s.nics {
			if n.Name() == opts.Name {
				return &tcpip.ErrDuplicateNICID{}
			}
		}
	}

	n := newNIC(s, id, ep, opts)
	for proto := range s.defaultForwardingEnabled {
		if _, err := n.setForwarding(proto, true); err != nil {
			panic(fmt.Sprintf("newNIC(%d, ...).setForwarding(%d, true): %s", id, proto, err))
		}
	}
	s.nics[id] = n
	ep.SetOnCloseAction(func() {
		s.RemoveNIC(id)
	})
	if !opts.Disabled {
		return n.enable()
	}

	return nil
}

// CreateNIC creates a NIC with the provided id and LinkEndpoint and calls
// LinkEndpoint.Attach to bind ep with a NetworkDispatcher.
func (s *Stack) CreateNIC(id tcpip.NICID, ep LinkEndpoint) tcpip.Error {
	return s.CreateNICWithOptions(id, ep, NICOptions{})
}

// GetLinkEndpointByName gets the link endpoint specified by name.
func (s *Stack) GetLinkEndpointByName(name string) LinkEndpoint {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, nic := range s.nics {
		if nic.Name() == name {
			linkEP, ok := nic.NetworkLinkEndpoint.(LinkEndpoint)
			if !ok {
				panic(fmt.Sprintf("unexpected NetworkLinkEndpoint(%#v) is not a LinkEndpoint", nic.NetworkLinkEndpoint))
			}
			return linkEP
		}
	}
	return nil
}

// EnableNIC enables the given NIC so that the link-layer endpoint can start
// delivering packets to it.
func (s *Stack) EnableNIC(id tcpip.NICID) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.enable()
}

// DisableNIC disables the given NIC.
func (s *Stack) DisableNIC(id tcpip.NICID) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	nic.disable()
	return nil
}

// CheckNIC checks if a NIC is usable.
func (s *Stack) CheckNIC(id tcpip.NICID) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return false
	}

	return nic.Enabled()
}

// RemoveNIC removes NIC and all related routes from the network stack.
func (s *Stack) RemoveNIC(id tcpip.NICID) tcpip.Error {
	s.mu.Lock()
	deferAct, err := s.removeNICLocked(id)
	s.mu.Unlock()
	if deferAct != nil {
		deferAct()
	}
	return err
}

// removeNICLocked removes NIC and all related routes from the network stack.
//
// +checklocks:s.mu
func (s *Stack) removeNICLocked(id tcpip.NICID) (func(), tcpip.Error) {
	nic, ok := s.nics[id]
	if !ok {
		return nil, &tcpip.ErrUnknownNICID{}
	}
	delete(s.nics, id)

	if nic.Primary != nil {
		b := nic.Primary.NetworkLinkEndpoint.(CoordinatorNIC)
		if err := b.DelNIC(nic); err != nil {
			return nil, err
		}
	}

	// Remove routes in-place. n tracks the number of routes written.
	s.routeMu.Lock()
	for r := s.routeTable.Front(); r != nil; {
		next := r.Next()
		if r.NIC == id {
			s.routeTable.Remove(r)
		}
		r = next
	}
	s.routeMu.Unlock()

	return nic.remove(true /* closeLinkEndpoint */)
}

// SetNICCoordinator sets a coordinator device.
func (s *Stack) SetNICCoordinator(id tcpip.NICID, mid tcpip.NICID) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	// Setting a coordinator for a coordinator NIC is not allowed.
	if _, ok := nic.NetworkLinkEndpoint.(CoordinatorNIC); ok {
		return &tcpip.ErrNoSuchFile{}
	}
	m, ok := s.nics[mid]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	b, ok := m.NetworkLinkEndpoint.(CoordinatorNIC)
	if !ok {
		return &tcpip.ErrNotSupported{}
	}
	if err := b.AddNIC(nic); err != nil {
		return err
	}
	nic.Primary = m
	return nil
}

// SetNICAddress sets the hardware address which is identified by the nic ID.
func (s *Stack) SetNICAddress(id tcpip.NICID, addr tcpip.LinkAddress) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	nic.NetworkLinkEndpoint.SetLinkAddress(addr)
	return nil
}

// SetNICName sets a NIC's name.
func (s *Stack) SetNICName(id tcpip.NICID, name string) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	nic.name = name
	return nil
}

// SetNICMTU sets a NIC's MTU.
func (s *Stack) SetNICMTU(id tcpip.NICID, mtu uint32) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	nic.NetworkLinkEndpoint.SetMTU(mtu)
	return nil
}

// NICInfo captures the name and addresses assigned to a NIC.
type NICInfo struct {
	Name              string
	LinkAddress       tcpip.LinkAddress
	ProtocolAddresses []tcpip.ProtocolAddress

	// Flags indicate the state of the NIC.
	Flags NICStateFlags

	// MTU is the maximum transmission unit.
	MTU uint32

	Stats tcpip.NICStats

	// NetworkStats holds the stats of each NetworkEndpoint bound to the NIC.
	NetworkStats map[tcpip.NetworkProtocolNumber]NetworkEndpointStats

	// Context is user-supplied data optionally supplied in CreateNICWithOptions.
	// See type NICOptions for more details.
	Context NICContext

	// ARPHardwareType holds the ARP Hardware type of the NIC. This is the
	// value sent in haType field of an ARP Request sent by this NIC and the
	// value expected in the haType field of an ARP response.
	ARPHardwareType header.ARPHardwareType

	// Forwarding holds the forwarding status for each network endpoint that
	// supports forwarding.
	Forwarding map[tcpip.NetworkProtocolNumber]bool

	// MulticastForwarding holds the forwarding status for each network endpoint
	// that supports multicast forwarding.
	MulticastForwarding map[tcpip.NetworkProtocolNumber]bool
}

// HasNIC returns true if the NICID is defined in the stack.
func (s *Stack) HasNIC(id tcpip.NICID) bool {
	s.mu.RLock()
	_, ok := s.nics[id]
	s.mu.RUnlock()
	return ok
}

// NICInfo returns a map of NICIDs to their associated information.
func (s *Stack) NICInfo() map[tcpip.NICID]NICInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()

	type forwardingFn func(tcpip.NetworkProtocolNumber) (bool, tcpip.Error)
	forwardingValue := func(forwardingFn forwardingFn, proto tcpip.NetworkProtocolNumber, nicID tcpip.NICID, fnName string) (forward bool, ok bool) {
		switch forwarding, err := forwardingFn(proto); err.(type) {
		case nil:
			return forwarding, true
		case *tcpip.ErrUnknownProtocol:
			panic(fmt.Sprintf("expected network protocol %d to be available on NIC %d", proto, nicID))
		case *tcpip.ErrNotSupported:
			// Not all network protocols support forwarding.
		default:
			panic(fmt.Sprintf("nic(id=%d).%s(%d): %s", nicID, fnName, proto, err))
		}
		return false, false
	}

	nics := make(map[tcpip.NICID]NICInfo)
	for id, nic := range s.nics {
		flags := NICStateFlags{
			Up:          true, // Netstack interfaces are always up.
			Running:     nic.Enabled(),
			Promiscuous: nic.Promiscuous(),
			Loopback:    nic.IsLoopback(),
		}

		netStats := make(map[tcpip.NetworkProtocolNumber]NetworkEndpointStats)
		for proto, netEP := range nic.networkEndpoints {
			netStats[proto] = netEP.Stats()
		}

		info := NICInfo{
			Name:                nic.name,
			LinkAddress:         nic.NetworkLinkEndpoint.LinkAddress(),
			ProtocolAddresses:   nic.primaryAddresses(),
			Flags:               flags,
			MTU:                 nic.NetworkLinkEndpoint.MTU(),
			Stats:               nic.stats.local,
			NetworkStats:        netStats,
			Context:             nic.context,
			ARPHardwareType:     nic.NetworkLinkEndpoint.ARPHardwareType(),
			Forwarding:          make(map[tcpip.NetworkProtocolNumber]bool),
			MulticastForwarding: make(map[tcpip.NetworkProtocolNumber]bool),
		}

		for proto := range s.networkProtocols {
			if forwarding, ok := forwardingValue(nic.forwarding, proto, id, "forwarding"); ok {
				info.Forwarding[proto] = forwarding
			}

			if multicastForwarding, ok := forwardingValue(nic.multicastForwarding, proto, id, "multicastForwarding"); ok {
				info.MulticastForwarding[proto] = multicastForwarding
			}
		}

		nics[id] = info
	}
	return nics
}

// NICStateFlags holds information about the state of an NIC.
type NICStateFlags struct {
	// Up indicates whether the interface is running.
	Up bool

	// Running indicates whether resources are allocated.
	Running bool

	// Promiscuous indicates whether the interface is in promiscuous mode.
	Promiscuous bool

	// Loopback indicates whether the interface is a loopback.
	Loopback bool
}

// AddProtocolAddress adds an address to the specified NIC, possibly with extra
// properties.
func (s *Stack) AddProtocolAddress(id tcpip.NICID, protocolAddress tcpip.ProtocolAddress, properties AddressProperties) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.addAddress(protocolAddress, properties)
}

// RemoveAddress removes an existing network-layer address from the specified
// NIC.
func (s *Stack) RemoveAddress(id tcpip.NICID, addr tcpip.Address) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nic, ok := s.nics[id]; ok {
		return nic.removeAddress(addr)
	}

	return &tcpip.ErrUnknownNICID{}
}

// SetAddressLifetimes sets informational preferred and valid lifetimes, and
// whether the address should be preferred or deprecated.
func (s *Stack) SetAddressLifetimes(id tcpip.NICID, addr tcpip.Address, lifetimes AddressLifetimes) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nic, ok := s.nics[id]; ok {
		return nic.setAddressLifetimes(addr, lifetimes)
	}

	return &tcpip.ErrUnknownNICID{}
}

// AllAddresses returns a map of NICIDs to their protocol addresses (primary
// and non-primary).
func (s *Stack) AllAddresses() map[tcpip.NICID][]tcpip.ProtocolAddress {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nics := make(map[tcpip.NICID][]tcpip.ProtocolAddress)
	for id, nic := range s.nics {
		nics[id] = nic.allPermanentAddresses()
	}
	return nics
}

// GetMainNICAddress returns the first non-deprecated primary address and prefix
// for the given NIC and protocol. If no non-deprecated primary addresses exist,
// a deprecated address will be returned. If no deprecated addresses exist, the
// zero value will be returned.
func (s *Stack) GetMainNICAddress(id tcpip.NICID, protocol tcpip.NetworkProtocolNumber) (tcpip.AddressWithPrefix, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return tcpip.AddressWithPrefix{}, &tcpip.ErrUnknownNICID{}
	}

	return nic.PrimaryAddress(protocol)
}

func (s *Stack) getAddressEP(nic *nic, localAddr, remoteAddr, srcHint tcpip.Address, netProto tcpip.NetworkProtocolNumber) AssignableAddressEndpoint {
	if localAddr.BitLen() == 0 {
		return nic.primaryEndpoint(netProto, remoteAddr, srcHint)
	}
	return nic.findEndpoint(netProto, localAddr, CanBePrimaryEndpoint)
}

// NewRouteForMulticast returns a Route that may be used to forward multicast
// packets.
//
// Returns nil if validation fails.
func (s *Stack) NewRouteForMulticast(nicID tcpip.NICID, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber) *Route {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[nicID]
	if !ok || !nic.Enabled() {
		return nil
	}

	if addressEndpoint := s.getAddressEP(nic, tcpip.Address{} /* localAddr */, remoteAddr, tcpip.Address{} /* srcHint */, netProto); addressEndpoint != nil {
		return constructAndValidateRoute(netProto, addressEndpoint, nic, nic, tcpip.Address{} /* gateway */, tcpip.Address{} /* localAddr */, remoteAddr, s.handleLocal, false /* multicastLoop */, 0 /* mtu */)
	}
	return nil
}

// findLocalRouteFromNICRLocked is like findLocalRouteRLocked but finds a route
// from the specified NIC.
//
// +checklocksread:s.mu
func (s *Stack) findLocalRouteFromNICRLocked(localAddressNIC *nic, localAddr, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber) *Route {
	localAddressEndpoint := localAddressNIC.getAddressOrCreateTempInner(netProto, localAddr, false /* createTemp */, NeverPrimaryEndpoint)
	if localAddressEndpoint == nil {
		return nil
	}

	var outgoingNIC *nic
	// Prefer a local route to the same interface as the local address.
	if localAddressNIC.hasAddress(netProto, remoteAddr) {
		outgoingNIC = localAddressNIC
	}

	// If the remote address isn't owned by the local address's NIC, check all
	// NICs.
	if outgoingNIC == nil {
		for _, nic := range s.nics {
			if nic.hasAddress(netProto, remoteAddr) {
				outgoingNIC = nic
				break
			}
		}
	}

	// If the remote address is not owned by the stack, we can't return a local
	// route.
	if outgoingNIC == nil {
		localAddressEndpoint.DecRef()
		return nil
	}

	r := makeLocalRoute(
		netProto,
		localAddr,
		remoteAddr,
		outgoingNIC,
		localAddressNIC,
		localAddressEndpoint,
	)

	if r.IsOutboundBroadcast() {
		r.Release()
		return nil
	}

	return r
}

// findLocalRouteRLocked returns a local route.
//
// A local route is a route to some remote address which the stack owns. That
// is, a local route is a route where packets never have to leave the stack.
//
// +checklocksread:s.mu
func (s *Stack) findLocalRouteRLocked(localAddressNICID tcpip.NICID, localAddr, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber) *Route {
	if localAddr.BitLen() == 0 {
		localAddr = remoteAddr
	}

	if localAddressNICID == 0 {
		for _, localAddressNIC := range s.nics {
			if r := s.findLocalRouteFromNICRLocked(localAddressNIC, localAddr, remoteAddr, netProto); r != nil {
				return r
			}
		}

		return nil
	}

	if localAddressNIC, ok := s.nics[localAddressNICID]; ok {
		return s.findLocalRouteFromNICRLocked(localAddressNIC, localAddr, remoteAddr, netProto)
	}

	return nil
}

// HandleLocal returns true if non-loopback interfaces are allowed to loop packets.
func (s *Stack) HandleLocal() bool {
	return s.handleLocal
}

func isNICForwarding(nic *nic, proto tcpip.NetworkProtocolNumber) bool {
	switch forwarding, err := nic.forwarding(proto); err.(type) {
	case nil:
		return forwarding
	case *tcpip.ErrUnknownProtocol:
		panic(fmt.Sprintf("expected network protocol %d to be available on NIC %d", proto, nic.ID()))
	case *tcpip.ErrNotSupported:
		// Not all network protocols support forwarding.
		return false
	default:
		panic(fmt.Sprintf("nic(id=%d).forwarding(%d): %s", nic.ID(), proto, err))
	}
}

// findRouteWithLocalAddrFromAnyInterfaceRLocked returns a route to the given
// destination address, leaving through the given NIC.
//
// Rather than preferring to find a route that uses a local address assigned to
// the outgoing interface, it finds any NIC that holds a matching local address
// endpoint.
//
// +checklocksread:s.mu
func (s *Stack) findRouteWithLocalAddrFromAnyInterfaceRLocked(outgoingNIC *nic, localAddr, remoteAddr, srcHint, gateway tcpip.Address, netProto tcpip.NetworkProtocolNumber, multicastLoop bool, mtu uint32) *Route {
	for _, aNIC := range s.nics {
		addressEndpoint := s.getAddressEP(aNIC, localAddr, remoteAddr, srcHint, netProto)
		if addressEndpoint == nil {
			continue
		}

		if r := constructAndValidateRoute(netProto, addressEndpoint, aNIC /* localAddressNIC */, outgoingNIC, gateway, localAddr, remoteAddr, s.handleLocal, multicastLoop, mtu); r != nil {
			return r
		}
	}
	return nil
}

// FindRoute creates a route to the given destination address, leaving through
// the given NIC and local address (if provided).
//
// If a NIC is not specified, the returned route will leave through the same
// NIC as the NIC that has the local address assigned when forwarding is
// disabled. If forwarding is enabled and the NIC is unspecified, the route may
// leave through any interface unless the route is link-local.
//
// If no local address is provided, the stack will select a local address. If no
// remote address is provided, the stack will use a remote address equal to the
// local address.
func (s *Stack) FindRoute(id tcpip.NICID, localAddr, remoteAddr tcpip.Address, netProto tcpip.NetworkProtocolNumber, multicastLoop bool) (*Route, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Reject attempts to use unsupported protocols.
	if !s.CheckNetworkProtocol(netProto) {
		return nil, &tcpip.ErrUnknownProtocol{}
	}

	isLinkLocal := header.IsV6LinkLocalUnicastAddress(remoteAddr) || header.IsV6LinkLocalMulticastAddress(remoteAddr)
	isLocalBroadcast := remoteAddr == header.IPv4Broadcast
	isMulticast := header.IsV4MulticastAddress(remoteAddr) || header.IsV6MulticastAddress(remoteAddr)
	isLoopback := header.IsV4LoopbackAddress(remoteAddr) || header.IsV6LoopbackAddress(remoteAddr)
	needRoute := !(isLocalBroadcast || isMulticast || isLinkLocal || isLoopback)

	if s.handleLocal && !isMulticast && !isLocalBroadcast {
		if r := s.findLocalRouteRLocked(id, localAddr, remoteAddr, netProto); r != nil {
			return r, nil
		}
	}

	// If the interface is specified and we do not need a route, return a route
	// through the interface if the interface is valid and enabled.
	if id != 0 && !needRoute {
		if nic, ok := s.nics[id]; ok && nic.Enabled() {
			if addressEndpoint := s.getAddressEP(nic, localAddr, remoteAddr, tcpip.Address{} /* srcHint */, netProto); addressEndpoint != nil {
				return makeRoute(
					netProto,
					tcpip.Address{}, /* gateway */
					localAddr,
					remoteAddr,
					nic, /* outgoingNIC */
					nic, /* localAddressNIC*/
					addressEndpoint,
					s.handleLocal,
					multicastLoop,
					0, /* mtu */
				), nil
			}
		}

		if isLoopback {
			return nil, &tcpip.ErrBadLocalAddress{}
		}
		return nil, &tcpip.ErrNetworkUnreachable{}
	}

	onlyGlobalAddresses := !header.IsV6LinkLocalUnicastAddress(localAddr) && !isLinkLocal

	// Find a route to the remote with the route table.
	var chosenRoute tcpip.Route
	if r := func() *Route {
		s.routeMu.RLock()
		defer s.routeMu.RUnlock()

		for route := s.routeTable.Front(); route != nil; route = route.Next() {
			if remoteAddr.BitLen() != 0 && !route.Destination.Contains(remoteAddr) {
				continue
			}

			nic, ok := s.nics[route.NIC]
			if !ok || !nic.Enabled() {
				continue
			}

			if id == 0 || id == route.NIC {
				if addressEndpoint := s.getAddressEP(nic, localAddr, remoteAddr, route.SourceHint, netProto); addressEndpoint != nil {
					var gateway tcpip.Address
					if needRoute {
						gateway = route.Gateway
					}
					r := constructAndValidateRoute(netProto, addressEndpoint, nic /* outgoingNIC */, nic /* outgoingNIC */, gateway, localAddr, remoteAddr, s.handleLocal, multicastLoop, route.MTU)
					if r == nil {
						panic(fmt.Sprintf("non-forwarding route validation failed with route table entry = %#v, id = %d, localAddr = %s, remoteAddr = %s", route, id, localAddr, remoteAddr))
					}
					return r
				}
			}

			// If the stack has forwarding enabled, we haven't found a valid route to
			// the remote address yet, and we are routing locally generated traffic,
			// keep track of the first valid route. We keep iterating because we
			// prefer routes that let us use a local address that is assigned to the
			// outgoing interface. There is no requirement to do this from any RFC
			// but simply a choice made to better follow a strong host model which
			// the netstack follows at the time of writing.
			//
			// Note that for incoming traffic that we are forwarding (for which the
			// NIC and local address are unspecified), we do not keep iterating, as
			// there is no reason to prefer routes that let us use a local address
			// when routing forwarded (as opposed to locally-generated) traffic.
			locallyGenerated := (id != 0 || localAddr != tcpip.Address{})
			if onlyGlobalAddresses && chosenRoute.Equal(tcpip.Route{}) && isNICForwarding(nic, netProto) {
				if locallyGenerated {
					chosenRoute = *route
					continue
				}

				if r := s.findRouteWithLocalAddrFromAnyInterfaceRLocked(nic, localAddr, remoteAddr, route.SourceHint, route.Gateway, netProto, multicastLoop, route.MTU); r != nil {
					return r
				}
			}
		}

		return nil
	}(); r != nil {
		return r, nil
	}

	if !chosenRoute.Equal(tcpip.Route{}) {
		// At this point we know the stack has forwarding enabled since chosenRoute is
		// only set when forwarding is enabled.
		nic, ok := s.nics[chosenRoute.NIC]
		if !ok {
			// If the route's NIC was invalid, we should not have chosen the route.
			panic(fmt.Sprintf("chosen route must have a valid NIC with ID = %d", chosenRoute.NIC))
		}

		var gateway tcpip.Address
		if needRoute {
			gateway = chosenRoute.Gateway
		}

		// Use the specified NIC to get the local address endpoint.
		if id != 0 {
			if aNIC, ok := s.nics[id]; ok {
				if addressEndpoint := s.getAddressEP(aNIC, localAddr, remoteAddr, chosenRoute.SourceHint, netProto); addressEndpoint != nil {
					if r := constructAndValidateRoute(netProto, addressEndpoint, aNIC /* localAddressNIC */, nic /* outgoingNIC */, gateway, localAddr, remoteAddr, s.handleLocal, multicastLoop, chosenRoute.MTU); r != nil {
						return r, nil
					}
				}
			}

			// TODO(https://gvisor.dev/issues/8105): This should be ErrNetworkUnreachable.
			return nil, &tcpip.ErrHostUnreachable{}
		}

		if id == 0 {
			// If an interface is not specified, try to find a NIC that holds the local
			// address endpoint to construct a route.
			if r := s.findRouteWithLocalAddrFromAnyInterfaceRLocked(nic, localAddr, remoteAddr, chosenRoute.SourceHint, gateway, netProto, multicastLoop, chosenRoute.MTU); r != nil {
				return r, nil
			}
		}
	}

	if needRoute {
		// TODO(https://gvisor.dev/issues/8105): This should be ErrNetworkUnreachable.
		return nil, &tcpip.ErrHostUnreachable{}
	}
	if header.IsV6LoopbackAddress(remoteAddr) {
		return nil, &tcpip.ErrBadLocalAddress{}
	}
	// TODO(https://gvisor.dev/issues/8105): This should be ErrNetworkUnreachable.
	return nil, &tcpip.ErrNetworkUnreachable{}
}

// CheckNetworkProtocol checks if a given network protocol is enabled in the
// stack.
func (s *Stack) CheckNetworkProtocol(protocol tcpip.NetworkProtocolNumber) bool {
	_, ok := s.networkProtocols[protocol]
	return ok
}

// CheckDuplicateAddress performs duplicate address detection for the address on
// the specified interface.
func (s *Stack) CheckDuplicateAddress(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address, h DADCompletionHandler) (DADCheckAddressDisposition, tcpip.Error) {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()

	if !ok {
		return 0, &tcpip.ErrUnknownNICID{}
	}

	return nic.checkDuplicateAddress(protocol, addr, h)
}

// CheckLocalAddress determines if the given local address exists, and if it
// does, returns the id of the NIC it's bound to. Returns 0 if the address
// does not exist.
func (s *Stack) CheckLocalAddress(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) tcpip.NICID {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// If a NIC is specified, use its NIC id.
	if nicID != 0 {
		nic, ok := s.nics[nicID]
		if !ok {
			return 0
		}
		// In IPv4, linux only checks the interface. If it matches, then it does
		// not bother with the address.
		// https://github.com/torvalds/linux/blob/15205c2829ca2cbb5ece5ceaafe1171a8470e62b/net/ipv4/igmp.c#L1829-L1837
		if protocol == header.IPv4ProtocolNumber {
			return nic.id
		}
		if nic.CheckLocalAddress(protocol, addr) {
			return nic.id
		}
		return 0
	}

	// Go through all the NICs.
	for _, nic := range s.nics {
		if nic.CheckLocalAddress(protocol, addr) {
			return nic.id
		}
	}

	return 0
}

// SetPromiscuousMode enables or disables promiscuous mode in the given NIC.
func (s *Stack) SetPromiscuousMode(nicID tcpip.NICID, enable bool) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[nicID]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	nic.setPromiscuousMode(enable)

	return nil
}

// SetSpoofing enables or disables address spoofing in the given NIC, allowing
// endpoints to bind to any address in the NIC.
func (s *Stack) SetSpoofing(nicID tcpip.NICID, enable bool) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[nicID]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	nic.setSpoofing(enable)

	return nil
}

// LinkResolutionResult is the result of a link address resolution attempt.
type LinkResolutionResult struct {
	LinkAddress tcpip.LinkAddress
	Err         tcpip.Error
}

// GetLinkAddress finds the link address corresponding to a network address.
//
// Returns ErrNotSupported if the stack is not configured with a link address
// resolver for the specified network protocol.
//
// Returns ErrWouldBlock if the link address is not readily available, along
// with a notification channel for the caller to block on. Triggers address
// resolution asynchronously.
//
// onResolve will be called either immediately, if resolution is not required,
// or when address resolution is complete, with the resolved link address and
// whether resolution succeeded.
//
// If specified, the local address must be an address local to the interface
// the neighbor cache belongs to. The local address is the source address of
// a packet prompting NUD/link address resolution.
func (s *Stack) GetLinkAddress(nicID tcpip.NICID, addr, localAddr tcpip.Address, protocol tcpip.NetworkProtocolNumber, onResolve func(LinkResolutionResult)) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.getLinkAddress(addr, localAddr, protocol, onResolve)
}

// Neighbors returns all IP to MAC address associations.
func (s *Stack) Neighbors(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber) ([]NeighborEntry, tcpip.Error) {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()

	if !ok {
		return nil, &tcpip.ErrUnknownNICID{}
	}

	return nic.neighbors(protocol)
}

// AddStaticNeighbor statically associates an IP address to a MAC address.
func (s *Stack) AddStaticNeighbor(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address, linkAddr tcpip.LinkAddress) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()

	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.addStaticNeighbor(addr, protocol, linkAddr)
}

// RemoveNeighbor removes an IP to MAC address association previously created
// either automatically or by AddStaticNeighbor. Returns ErrBadAddress if there
// is no association with the provided address.
func (s *Stack) RemoveNeighbor(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()

	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.removeNeighbor(protocol, addr)
}

// ClearNeighbors removes all IP to MAC address associations.
func (s *Stack) ClearNeighbors(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()

	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.clearNeighbors(protocol)
}

// RegisterTransportEndpoint registers the given endpoint with the stack
// transport dispatcher. Received packets that match the provided id will be
// delivered to the given endpoint; specifying a nic is optional, but
// nic-specific IDs have precedence over global ones.
func (s *Stack) RegisterTransportEndpoint(netProtos []tcpip.NetworkProtocolNumber, protocol tcpip.TransportProtocolNumber, id TransportEndpointID, ep TransportEndpoint, flags ports.Flags, bindToDevice tcpip.NICID) tcpip.Error {
	return s.demux.registerEndpoint(netProtos, protocol, id, ep, flags, bindToDevice)
}

// CheckRegisterTransportEndpoint checks if an endpoint can be registered with
// the stack transport dispatcher.
func (s *Stack) CheckRegisterTransportEndpoint(netProtos []tcpip.NetworkProtocolNumber, protocol tcpip.TransportProtocolNumber, id TransportEndpointID, flags ports.Flags, bindToDevice tcpip.NICID) tcpip.Error {
	return s.demux.checkEndpoint(netProtos, protocol, id, flags, bindToDevice)
}

// UnregisterTransportEndpoint removes the endpoint with the given id from the
// stack transport dispatcher.
func (s *Stack) UnregisterTransportEndpoint(netProtos []tcpip.NetworkProtocolNumber, protocol tcpip.TransportProtocolNumber, id TransportEndpointID, ep TransportEndpoint, flags ports.Flags, bindToDevice tcpip.NICID) {
	s.demux.unregisterEndpoint(netProtos, protocol, id, ep, flags, bindToDevice)
}

// StartTransportEndpointCleanup removes the endpoint with the given id from
// the stack transport dispatcher. It also transitions it to the cleanup stage.
func (s *Stack) StartTransportEndpointCleanup(netProtos []tcpip.NetworkProtocolNumber, protocol tcpip.TransportProtocolNumber, id TransportEndpointID, ep TransportEndpoint, flags ports.Flags, bindToDevice tcpip.NICID) {
	s.cleanupEndpointsMu.Lock()
	s.cleanupEndpoints[ep] = struct{}{}
	s.cleanupEndpointsMu.Unlock()

	s.demux.unregisterEndpoint(netProtos, protocol, id, ep, flags, bindToDevice)
}

// CompleteTransportEndpointCleanup removes the endpoint from the cleanup
// stage.
func (s *Stack) CompleteTransportEndpointCleanup(ep TransportEndpoint) {
	s.cleanupEndpointsMu.Lock()
	delete(s.cleanupEndpoints, ep)
	s.cleanupEndpointsMu.Unlock()
}

// FindTransportEndpoint finds an endpoint that most closely matches the provided
// id. If no endpoint is found it returns nil.
func (s *Stack) FindTransportEndpoint(netProto tcpip.NetworkProtocolNumber, transProto tcpip.TransportProtocolNumber, id TransportEndpointID, nicID tcpip.NICID) TransportEndpoint {
	return s.demux.findTransportEndpoint(netProto, transProto, id, nicID)
}

// RegisterRawTransportEndpoint registers the given endpoint with the stack
// transport dispatcher. Received packets that match the provided transport
// protocol will be delivered to the given endpoint.
func (s *Stack) RegisterRawTransportEndpoint(netProto tcpip.NetworkProtocolNumber, transProto tcpip.TransportProtocolNumber, ep RawTransportEndpoint) tcpip.Error {
	return s.demux.registerRawEndpoint(netProto, transProto, ep)
}

// UnregisterRawTransportEndpoint removes the endpoint for the transport
// protocol from the stack transport dispatcher.
func (s *Stack) UnregisterRawTransportEndpoint(netProto tcpip.NetworkProtocolNumber, transProto tcpip.TransportProtocolNumber, ep RawTransportEndpoint) {
	s.demux.unregisterRawEndpoint(netProto, transProto, ep)
}

// RegisterRestoredEndpoint records e as an endpoint that has been restored on
// this stack.
func (s *Stack) RegisterRestoredEndpoint(e RestoredEndpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.restoredEndpoints = append(s.restoredEndpoints, e)
}

// RegisterResumableEndpoint records e as an endpoint that has to be resumed.
func (s *Stack) RegisterResumableEndpoint(e ResumableEndpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.resumableEndpoints = append(s.resumableEndpoints, e)
}

// RegisteredEndpoints returns all endpoints which are currently registered.
func (s *Stack) RegisteredEndpoints() []TransportEndpoint {
	s.mu.Lock()
	defer s.mu.Unlock()

	var es []TransportEndpoint
	for _, e := range s.demux.protocol {
		es = append(es, e.transportEndpoints()...)
	}
	return es
}

// CleanupEndpoints returns endpoints currently in the cleanup state.
func (s *Stack) CleanupEndpoints() []TransportEndpoint {
	s.cleanupEndpointsMu.Lock()
	defer s.cleanupEndpointsMu.Unlock()

	es := make([]TransportEndpoint, 0, len(s.cleanupEndpoints))
	for e := range s.cleanupEndpoints {
		es = append(es, e)
	}
	return es
}

// RestoreCleanupEndpoints adds endpoints to cleanup tracking. This is useful
// for restoring a stack after a save.
func (s *Stack) RestoreCleanupEndpoints(es []TransportEndpoint) {
	s.cleanupEndpointsMu.Lock()
	defer s.cleanupEndpointsMu.Unlock()

	for _, e := range es {
		s.cleanupEndpoints[e] = struct{}{}
	}
}

// Close closes all currently registered transport endpoints.
//
// Endpoints created or modified during this call may not get closed.
func (s *Stack) Close() {
	for _, e := range s.RegisteredEndpoints() {
		e.Abort()
	}
	for _, p := range s.transportProtocols {
		p.proto.Close()
	}
	for _, p := range s.networkProtocols {
		p.Close()
	}
}

// Wait waits for all transport and link endpoints to halt their worker
// goroutines.
//
// Endpoints created or modified during this call may not get waited on.
//
// Note that link endpoints must be stopped via an implementation specific
// mechanism.
func (s *Stack) Wait() {
	for _, e := range s.RegisteredEndpoints() {
		e.Wait()
	}
	for _, e := range s.CleanupEndpoints() {
		e.Wait()
	}
	for _, p := range s.transportProtocols {
		p.proto.Wait()
	}
	for _, p := range s.networkProtocols {
		p.Wait()
	}

	deferActs := make([]func(), 0)

	s.mu.Lock()
	for id, n := range s.nics {
		// Remove NIC to ensure that qDisc goroutines are correctly
		// terminated on stack teardown.
		act, _ := s.removeNICLocked(id)
		n.NetworkLinkEndpoint.Wait()
		if act != nil {
			deferActs = append(deferActs, act)
		}
	}
	s.mu.Unlock()

	for _, act := range deferActs {
		act()
	}
}

// Destroy destroys the stack with all endpoints.
func (s *Stack) Destroy() {
	s.Close()
	s.Wait()
}

// Pause pauses any protocol level background workers.
func (s *Stack) Pause() {
	for _, p := range s.transportProtocols {
		p.proto.Pause()
	}
}

func (s *Stack) getNICs() map[tcpip.NICID]*nic {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nics := s.nics
	return nics
}

// ReplaceConfig replaces config in the loaded stack.
func (s *Stack) ReplaceConfig(st *Stack) {
	if st == nil {
		panic("stack.Stack cannot be nil when netstack s/r is enabled")
	}

	// Update route table.
	s.SetRouteTable(st.GetRouteTable())

	// Update NICs.
	nics := st.getNICs()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nics = make(map[tcpip.NICID]*nic)
	for id, nic := range nics {
		nic.stack = s
		s.nics[id] = nic
		_ = s.NextNICID()
	}
	s.tables = st.tables
	s.nftables = st.nftables
}

// Restore restarts the stack after a restore. This must be called after the
// entire system has been restored.
func (s *Stack) Restore() {
	// RestoredEndpoint.Restore() may call other methods on s, so we can't hold
	// s.mu while restoring the endpoints.
	s.mu.Lock()
	eps := s.restoredEndpoints
	s.restoredEndpoints = nil
	saveRestoreEnabled := s.saveRestoreEnabled
	s.mu.Unlock()
	for _, e := range eps {
		e.Restore(s)
	}

	// Make sure all the endpoints are loaded correctly before resuming the
	// protocol level background workers.
	tcpip.AsyncLoading.Wait()

	// Now resume any protocol level background workers.
	for _, p := range s.transportProtocols {
		if saveRestoreEnabled {
			p.proto.Restore()
		} else {
			p.proto.Resume()
		}
	}
}

// Resume resumes the stack after a save.
func (s *Stack) Resume() {
	s.mu.Lock()
	eps := s.resumableEndpoints
	s.resumableEndpoints = nil
	s.mu.Unlock()
	for _, e := range eps {
		e.Resume()
	}
	// Now resume any protocol level background workers.
	for _, p := range s.transportProtocols {
		p.proto.Resume()
	}
}

// RegisterPacketEndpoint registers ep with the stack, causing it to receive
// all traffic of the specified netProto on the given NIC. If nicID is 0, it
// receives traffic from every NIC.
func (s *Stack) RegisterPacketEndpoint(nicID tcpip.NICID, netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) tcpip.Error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// If no NIC is specified, capture on all devices.
	if nicID == 0 {
		// Register with each NIC.
		for _, nic := range s.nics {
			nic.registerPacketEndpoint(netProto, ep)
		}
		return nil
	}

	// Capture on a specific device.
	nic, ok := s.nics[nicID]
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}
	nic.registerPacketEndpoint(netProto, ep)

	return nil
}

// UnregisterPacketEndpoint unregisters ep for packets of the specified
// netProto from the specified NIC. If nicID is 0, ep is unregistered from all
// NICs.
func (s *Stack) UnregisterPacketEndpoint(nicID tcpip.NICID, netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unregisterPacketEndpointLocked(nicID, netProto, ep)
}

// +checklocks:s.mu
func (s *Stack) unregisterPacketEndpointLocked(nicID tcpip.NICID, netProto tcpip.NetworkProtocolNumber, ep PacketEndpoint) {
	// If no NIC is specified, unregister on all devices.
	if nicID == 0 {
		// Unregister with each NIC.
		for _, nic := range s.nics {
			nic.unregisterPacketEndpoint(netProto, ep)
		}
		return
	}

	// Unregister in a single device.
	nic, ok := s.nics[nicID]
	if !ok {
		return
	}
	nic.unregisterPacketEndpoint(netProto, ep)
}

// WritePacketToRemote writes a payload on the specified NIC using the provided
// network protocol and remote link address.
func (s *Stack) WritePacketToRemote(nicID tcpip.NICID, remote tcpip.LinkAddress, netProto tcpip.NetworkProtocolNumber, payload buffer.Buffer) tcpip.Error {
	s.mu.Lock()
	nic, ok := s.nics[nicID]
	s.mu.Unlock()
	if !ok {
		return &tcpip.ErrUnknownDevice{}
	}
	pkt := NewPacketBuffer(PacketBufferOptions{
		ReserveHeaderBytes: int(nic.MaxHeaderLength()),
		Payload:            payload,
	})
	defer pkt.DecRef()
	pkt.NetworkProtocolNumber = netProto
	return nic.WritePacketToRemote(remote, pkt)
}

// WriteRawPacket writes data directly to the specified NIC without adding any
// headers.
func (s *Stack) WriteRawPacket(nicID tcpip.NICID, proto tcpip.NetworkProtocolNumber, payload buffer.Buffer) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[nicID]
	s.mu.RUnlock()
	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	pkt := NewPacketBuffer(PacketBufferOptions{
		Payload: payload,
	})
	defer pkt.DecRef()
	pkt.NetworkProtocolNumber = proto
	return nic.writeRawPacketWithLinkHeaderInPayload(pkt)
}

// NetworkProtocolInstance returns the protocol instance in the stack for the
// specified network protocol. This method is public for protocol implementers
// and tests to use.
func (s *Stack) NetworkProtocolInstance(num tcpip.NetworkProtocolNumber) NetworkProtocol {
	if p, ok := s.networkProtocols[num]; ok {
		return p
	}
	return nil
}

// TransportProtocolInstance returns the protocol instance in the stack for the
// specified transport protocol. This method is public for protocol implementers
// and tests to use.
func (s *Stack) TransportProtocolInstance(num tcpip.TransportProtocolNumber) TransportProtocol {
	if pState, ok := s.transportProtocols[num]; ok {
		return pState.proto
	}
	return nil
}

// JoinGroup joins the given multicast group on the given NIC.
func (s *Stack) JoinGroup(protocol tcpip.NetworkProtocolNumber, nicID tcpip.NICID, multicastAddr tcpip.Address) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nic, ok := s.nics[nicID]; ok {
		return nic.joinGroup(protocol, multicastAddr)
	}
	return &tcpip.ErrUnknownNICID{}
}

// LeaveGroup leaves the given multicast group on the given NIC.
func (s *Stack) LeaveGroup(protocol tcpip.NetworkProtocolNumber, nicID tcpip.NICID, multicastAddr tcpip.Address) tcpip.Error {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nic, ok := s.nics[nicID]; ok {
		return nic.leaveGroup(protocol, multicastAddr)
	}
	return &tcpip.ErrUnknownNICID{}
}

// IsInGroup returns true if the NIC with ID nicID has joined the multicast
// group multicastAddr.
func (s *Stack) IsInGroup(nicID tcpip.NICID, multicastAddr tcpip.Address) (bool, tcpip.Error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nic, ok := s.nics[nicID]; ok {
		return nic.isInGroup(multicastAddr), nil
	}
	return false, &tcpip.ErrUnknownNICID{}
}

// IPTables returns the stack's iptables.
func (s *Stack) IPTables() *IPTables {
	return s.tables
}

// NFTables returns the stack's nftables.
func (s *Stack) NFTables() NFTablesInterface {
	return s.nftables
}

// SetNFTables sets the stack's nftables.
func (s *Stack) SetNFTables(nft NFTablesInterface) {
	s.nftables = nft
}

// ICMPLimit returns the maximum number of ICMP messages that can be sent
// in one second.
func (s *Stack) ICMPLimit() rate.Limit {
	return s.icmpRateLimiter.Limit()
}

// SetICMPLimit sets the maximum number of ICMP messages that be sent
// in one second.
func (s *Stack) SetICMPLimit(newLimit rate.Limit) {
	s.icmpRateLimiter.SetLimit(newLimit)
}

// ICMPBurst returns the maximum number of ICMP messages that can be sent
// in a single burst.
func (s *Stack) ICMPBurst() int {
	return s.icmpRateLimiter.Burst()
}

// SetICMPBurst sets the maximum number of ICMP messages that can be sent
// in a single burst.
func (s *Stack) SetICMPBurst(burst int) {
	s.icmpRateLimiter.SetBurst(burst)
}

// AllowICMPMessage returns true if we the rate limiter allows at least one
// ICMP message to be sent at this instant.
func (s *Stack) AllowICMPMessage() bool {
	return s.icmpRateLimiter.Allow()
}

// GetNetworkEndpoint returns the NetworkEndpoint with the specified protocol
// number installed on the specified NIC.
func (s *Stack) GetNetworkEndpoint(nicID tcpip.NICID, proto tcpip.NetworkProtocolNumber) (NetworkEndpoint, tcpip.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nic, ok := s.nics[nicID]
	if !ok {
		return nil, &tcpip.ErrUnknownNICID{}
	}

	return nic.getNetworkEndpoint(proto), nil
}

// NUDConfigurations gets the per-interface NUD configurations.
func (s *Stack) NUDConfigurations(id tcpip.NICID, proto tcpip.NetworkProtocolNumber) (NUDConfigurations, tcpip.Error) {
	s.mu.RLock()
	nic, ok := s.nics[id]
	s.mu.RUnlock()

	if !ok {
		return NUDConfigurations{}, &tcpip.ErrUnknownNICID{}
	}

	return nic.nudConfigs(proto)
}

// SetNUDConfigurations sets the per-interface NUD configurations.
//
// Note, if c contains invalid NUD configuration values, it will be fixed to
// use default values for the erroneous values.
func (s *Stack) SetNUDConfigurations(id tcpip.NICID, proto tcpip.NetworkProtocolNumber, c NUDConfigurations) tcpip.Error {
	s.mu.RLock()
	nic, ok := s.nics[id]
	s.mu.RUnlock()

	if !ok {
		return &tcpip.ErrUnknownNICID{}
	}

	return nic.setNUDConfigs(proto, c)
}

// Seed returns a 32 bit value that can be used as a seed value.
//
// NOTE: The seed is generated once during stack initialization only.
func (s *Stack) Seed() uint32 {
	return s.seed
}

// InsecureRNG returns a reference to a pseudo random generator that can be used
// to generate random numbers as required. It is not cryptographically secure
// and should not be used for security sensitive work.
func (s *Stack) InsecureRNG() *rand.Rand {
	return s.insecureRNG
}

// SecureRNG returns the stack's cryptographically secure random number
// generator.
func (s *Stack) SecureRNG() cryptorand.RNG {
	return s.secureRNG
}

// FindNICNameFromID returns the name of the NIC for the given NICID.
func (s *Stack) FindNICNameFromID(id tcpip.NICID) string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	nic, ok := s.nics[id]
	if !ok {
		return ""
	}

	return nic.Name()
}

// ParseResult indicates the result of a parsing attempt.
type ParseResult int

const (
	// ParsedOK indicates that a packet was successfully parsed.
	ParsedOK ParseResult = iota

	// UnknownTransportProtocol indicates that the transport protocol is unknown.
	UnknownTransportProtocol

	// TransportLayerParseError indicates that the transport packet was not
	// successfully parsed.
	TransportLayerParseError
)

// ParsePacketBufferTransport parses the provided packet buffer's transport
// header.
func (s *Stack) ParsePacketBufferTransport(protocol tcpip.TransportProtocolNumber, pkt *PacketBuffer) ParseResult {
	pkt.TransportProtocolNumber = protocol
	// Parse the transport header if present.
	state, ok := s.transportProtocols[protocol]
	if !ok {
		return UnknownTransportProtocol
	}

	if !state.proto.Parse(pkt) {
		return TransportLayerParseError
	}

	return ParsedOK
}

// networkProtocolNumbers returns the network protocol numbers the stack is
// configured with.
func (s *Stack) networkProtocolNumbers() []tcpip.NetworkProtocolNumber {
	protos := make([]tcpip.NetworkProtocolNumber, 0, len(s.networkProtocols))
	for p := range s.networkProtocols {
		protos = append(protos, p)
	}
	return protos
}

func isSubnetBroadcastOnNIC(nic *nic, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) bool {
	addressEndpoint := nic.getAddressOrCreateTempInner(protocol, addr, false /* createTemp */, NeverPrimaryEndpoint)
	if addressEndpoint == nil {
		return false
	}

	subnet := addressEndpoint.Subnet()
	addressEndpoint.DecRef()
	return subnet.IsBroadcast(addr)
}

// IsSubnetBroadcast returns true if the provided address is a subnet-local
// broadcast address on the specified NIC and protocol.
//
// Returns false if the NIC is unknown or if the protocol is unknown or does
// not support addressing.
//
// If the NIC is not specified, the stack will check all NICs.
func (s *Stack) IsSubnetBroadcast(nicID tcpip.NICID, protocol tcpip.NetworkProtocolNumber, addr tcpip.Address) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if nicID != 0 {
		nic, ok := s.nics[nicID]
		if !ok {
			return false
		}

		return isSubnetBroadcastOnNIC(nic, protocol, addr)
	}

	for _, nic := range s.nics {
		if isSubnetBroadcastOnNIC(nic, protocol, addr) {
			return true
		}
	}

	return false
}

// PacketEndpointWriteSupported returns true iff packet endpoints support write
// operations.
func (s *Stack) PacketEndpointWriteSupported() bool {
	return s.packetEndpointWriteSupported
}

// SetNICStack moves the network device to the specified network namespace.
func (s *Stack) SetNICStack(id tcpip.NICID, peer *Stack) (tcpip.NICID, tcpip.Error) {
	s.mu.Lock()
	nic, ok := s.nics[id]
	if !ok {
		s.mu.Unlock()
		return 0, &tcpip.ErrUnknownNICID{}
	}
	if s == peer {
		s.mu.Unlock()
		return id, nil
	}
	delete(s.nics, id)

	// Remove routes in-place. n tracks the number of routes written.
	s.RemoveRoutes(func(r tcpip.Route) bool { return r.NIC == id })
	ne := nic.NetworkLinkEndpoint.(LinkEndpoint)
	deferAct, err := nic.remove(false /* closeLinkEndpoint */)
	s.mu.Unlock()
	if deferAct != nil {
		deferAct()
	}
	if err != nil {
		return 0, err
	}

	id = tcpip.NICID(peer.NextNICID())
	return id, peer.CreateNICWithOptions(id, ne, NICOptions{Name: nic.Name()})
}

// EnableSaveRestore marks the saveRestoreEnabled to true.
func (s *Stack) EnableSaveRestore() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.saveRestoreEnabled = true
}

// IsSaveRestoreEnabled returns true if save restore is enabled for the stack.
func (s *Stack) IsSaveRestoreEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.saveRestoreEnabled
}

// contextID is this package's type for context.Context.Value keys.
type contextID int

const (
	// CtxRestoreStack is a Context.Value key for the stack to be used in restore.
	CtxRestoreStack contextID = iota
)

// RestoreStackFromContext returns the stack to be used during restore.
func RestoreStackFromContext(ctx context.Context) *Stack {
	return ctx.Value(CtxRestoreStack).(*Stack)
}
