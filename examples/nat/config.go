// Copyright 2017-2018 Intel Corporation.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package nat

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/intel-go/nff-go/common"
	"github.com/intel-go/nff-go/flow"
	"github.com/intel-go/nff-go/packet"
)

type terminationDirection uint8
type interfaceType int

const (
	pri2pub terminationDirection = 0x0f
	pub2pri terminationDirection = 0xf0

	iPUBLIC  interfaceType = 0
	iPRIVATE interfaceType = 1

	dirDROP = 0
	dirSEND = 1
	dirKNI  = 2

	connectionTimeout time.Duration = 1 * time.Minute
	portReuseTimeout  time.Duration = 1 * time.Second
)

type hostPort struct {
	Addr uint32
	Port uint16
}

type protocolId uint8

type forwardedPort struct {
	Port         uint16     `json:"port"`
	Destination  hostPort   `json:"destination"`
	Protocol     protocolId `json:"protocol"`
	forwardToKNI bool
}

var protocolIdLookup map[string]protocolId = map[string]protocolId{
	"TCP": common.TCPNumber,
	"UDP": common.UDPNumber,
}

func (out *protocolId) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	result, ok := protocolIdLookup[s]
	if !ok {
		return errors.New("Bad protocol name: " + s)
	}

	*out = result
	return nil
}

type ipv4Subnet struct {
	Addr uint32
	Mask uint32
}

func (fp *forwardedPort) String() string {
	return fmt.Sprintf("Port:%d, Destination:%+v, Protocol: %d", fp.Port, packet.IPv4ToString(fp.Destination.Addr), fp.Protocol)
}

func (subnet *ipv4Subnet) String() string {
	// Count most significant set bits
	mask := uint32(1) << 31
	i := 0
	for ; i <= 32; i++ {
		if subnet.Mask&mask == 0 {
			break
		}
		mask >>= 1
	}
	return packet.IPv4ToString(subnet.Addr) + "/" + strconv.Itoa(i)
}

func (subnet *ipv4Subnet) checkAddrWithingSubnet(addr uint32) bool {
	return addr&subnet.Mask == subnet.Addr&subnet.Mask
}

type macAddress [common.EtherAddrLen]uint8

type portMapEntry struct {
	lastused             time.Time
	addr                 uint32
	finCount             uint8
	terminationDirection terminationDirection
	static               bool
}

// Type describing a network port
type ipv4Port struct {
	Index         uint16          `json:"index"`
	Subnet        ipv4Subnet      `json:"subnet"`
	Vlan          uint16          `json:"vlan-tag"`
	KNIName       string          `json:"kni-name"`
	ForwardPorts  []forwardedPort `json:"forward-ports"`
	SrcMACAddress macAddress
	Type          interfaceType
	// Pointer to an opposite port in a pair
	opposite *ipv4Port
	// Map of allocated IP ports on public interface
	portmap [][]portMapEntry
	// Main lookup table which contains entries for packets coming at this port
	translationTable []*sync.Map
	// ARP lookup table
	arpTable sync.Map
	// Debug dump stuff
	fdump    [dirKNI + 1]*os.File
	dumpsync [dirKNI + 1]sync.Mutex
}

// Config for one port pair.
type portPair struct {
	PrivatePort ipv4Port `json:"private-port"`
	PublicPort  ipv4Port `json:"public-port"`
	// Synchronization point for lookup table modifications
	mutex sync.Mutex
	// Port that was allocated last
	lastport int
}

// Config for NAT.
type Config struct {
	PortPairs []portPair `json:"port-pairs"`
}

// Type used to pass handler index to translation functions.
type pairIndex struct {
	index int
}

var (
	// Natconfig is a config file.
	Natconfig *Config
	// CalculateChecksum is a flag whether checksums should be
	// calculated for modified packets.
	NoCalculateChecksum bool
	// HWTXChecksum is a flag whether checksums calculation should be
	// offloaded to HW.
	NoHWTXChecksum bool
	NeedKNI        bool

	// Debug variables
	debugDump = false
	debugDrop = false
)

func (pi pairIndex) Copy() interface{} {
	return pairIndex{
		index: pi.index,
	}
}

func (pi pairIndex) Delete() {
}

func ConvertIPv4(in []byte) (uint32, error) {
	if in == nil || len(in) > 4 {
		return 0, errors.New("Only IPv4 addresses are supported now")
	}

	addr := (uint32(in[0]) << 24) | (uint32(in[1]) << 16) |
		(uint32(in[2]) << 8) | uint32(in[3])

	return addr, nil
}

// UnmarshalJSON parses ipv 4 subnet details.
func (out *ipv4Subnet) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	if ip, ipnet, err := net.ParseCIDR(s); err == nil {
		if out.Addr, err = ConvertIPv4(ip.To4()); err != nil {
			return err
		}
		if out.Mask, err = ConvertIPv4(ipnet.Mask); err != nil {
			return err
		}
		return nil
	}

	if ip := net.ParseIP(s); ip != nil {
		var err error
		if out.Addr, err = ConvertIPv4(ip.To4()); err != nil {
			return err
		}
		out.Mask = 0xffffffff
		return nil
	}
	return errors.New("Failed to parse address " + s)
}

// UnmarshalJSON parses ipv4 host:port string. Port may be omitted and
// is set to zero in this case.
func (out *hostPort) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	hostStr, portStr, err := net.SplitHostPort(s)
	if err != nil {
		return err
	}

	ipArray := net.ParseIP(hostStr)
	if ipArray == nil {
		return errors.New("Bad IPv4 address specified: " + hostStr)
	}
	out.Addr, err = ConvertIPv4(ipArray.To4())
	if err != nil {
		return err
	}

	if portStr != "" {
		port, err := strconv.ParseInt(portStr, 10, 32)
		if err != nil {
			return err
		}
		out.Port = uint16(port)
	} else {
		out.Port = 0
	}

	return nil
}

// UnmarshalJSON parses MAC address.
func (out *macAddress) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}

	hw, err := net.ParseMAC(s)
	if err != nil {
		return err
	}

	copy(out[:], hw)
	return nil
}

// ReadConfig function reads and parses config file
func ReadConfig(fileName string) error {
	file, err := os.Open(fileName)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(file)

	err = decoder.Decode(&Natconfig)
	if err != nil {
		return err
	}

	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]

		pp.PrivatePort.Type = iPRIVATE
		pp.PublicPort.Type = iPUBLIC

		if pp.PrivatePort.Vlan == 0 && pp.PublicPort.Vlan != 0 {
			return errors.New("Private port with index " +
				strconv.Itoa(int(pp.PrivatePort.Index)) +
				" has zero vlan tag while public port with index " +
				strconv.Itoa(int(pp.PublicPort.Index)) +
				" has non-zero vlan tag. Transition between VLAN-enabled and VLAN-disabled networks is not supported yet.")
		} else if pp.PrivatePort.Vlan != 0 && pp.PublicPort.Vlan == 0 {
			return errors.New("Private port with index " +
				strconv.Itoa(int(pp.PrivatePort.Index)) +
				" has non-zero vlan tag while public port with index " +
				strconv.Itoa(int(pp.PublicPort.Index)) +
				" has zero vlan tag. Transition between VLAN-enabled and VLAN-disabled networks is not supported yet.")
		}

		port := &pp.PrivatePort
		opposite := &pp.PublicPort
		for pi := 0; pi < 2; pi++ {
			for fpi := range port.ForwardPorts {
				fp := &port.ForwardPorts[fpi]
				if fp.Destination.Addr == 0 {
					if port.KNIName == "" {
						return errors.New("Port with index " +
							strconv.Itoa(int(port.Index)) +
							" should have \"kni-name\" setting if you want to forward packets to KNI address 0.0.0.0")
					}
					fp.forwardToKNI = true
					if fp.Destination.Port != fp.Port {
						return errors.New("When address 0.0.0.0 is specified, it means that packets are forwarded to KNI interface. In this case destination port should be equal to forwarded port. You have different values: " +
							strconv.Itoa(int(fp.Port)) + " and " +
							strconv.Itoa(int(fp.Destination.Port)))
					}
					NeedKNI = true
				} else {
					if pi == 0 {
						return errors.New("Only KNI port forwarding is allowed on private port. All translated connections from private to public network can be initiated without any forwarding rules.")
					}
					if !opposite.Subnet.checkAddrWithingSubnet(fp.Destination.Addr) {
						return errors.New("Destination address " +
							packet.IPv4ToString(fp.Destination.Addr) +
							" should be within subnet " +
							opposite.Subnet.String())
					}
					if fp.Destination.Port == 0 {
						fp.Destination.Port = fp.Port
					}
				}
			}
			port = &pp.PublicPort
			opposite = &pp.PrivatePort
		}
	}

	return nil
}

// Reads MAC addresses for local interfaces into pair ports.
func (pp *portPair) initLocalMACs() {
	pp.PublicPort.SrcMACAddress = flow.GetPortMACAddress(pp.PublicPort.Index)
	pp.PrivatePort.SrcMACAddress = flow.GetPortMACAddress(pp.PrivatePort.Index)
}

func (port *ipv4Port) allocatePublicPortPortMap() {
	port.portmap = make([][]portMapEntry, common.UDPNumber+1)
	port.portmap[common.ICMPNumber] = make([]portMapEntry, portEnd)
	port.portmap[common.TCPNumber] = make([]portMapEntry, portEnd)
	port.portmap[common.UDPNumber] = make([]portMapEntry, portEnd)
}

func (port *ipv4Port) allocateLookupMap() {
	port.translationTable = make([]*sync.Map, common.UDPNumber+1)
	for i := range port.translationTable {
		port.translationTable[i] = new(sync.Map)
	}
}

func (port *ipv4Port) initPublicPortPortForwardingEntries() {
	// Initialize port forwarding rules on public interface
	for _, fp := range port.ForwardPorts {
		keyEntry := Tuple{
			addr: port.Subnet.Addr,
			port: fp.Port,
		}
		valEntry := Tuple{
			addr: fp.Destination.Addr,
			port: fp.Destination.Port,
		}
		port.translationTable[fp.Protocol].Store(keyEntry, valEntry)
		if fp.Destination.Addr != 0 {
			port.opposite.translationTable[fp.Protocol].Store(valEntry, keyEntry)
		}
		port.portmap[fp.Protocol][fp.Port] = portMapEntry{
			lastused:             time.Now(),
			addr:                 fp.Destination.Addr,
			finCount:             0,
			terminationDirection: 0,
			static:               true,
		}
	}
}

// InitFlows initializes flow graph for all interface pairs.
func InitFlows() {
	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]

		pp.PublicPort.opposite = &pp.PrivatePort
		pp.PrivatePort.opposite = &pp.PublicPort

		// Init port pairs state
		pp.initLocalMACs()
		pp.PrivatePort.allocateLookupMap()
		pp.PublicPort.allocateLookupMap()
		pp.PublicPort.allocatePublicPortPortMap()
		pp.lastport = portStart
		pp.PublicPort.initPublicPortPortForwardingEntries()

		// Handler context with handler index
		context := new(pairIndex)
		context.index = i

		var fromPubKNI, fromPrivKNI, toPub, toPriv *flow.Flow
		var pubKNI, privKNI *flow.Kni
		var outsPub = uint(2)
		var outsPriv = uint(2)

		// Initialize public to private flow
		publicToPrivate, err := flow.SetReceiver(pp.PublicPort.Index)
		flow.CheckFatal(err)
		if pp.PublicPort.KNIName != "" {
			outsPub = 3
		}
		pubTranslationOut, err := flow.SetSplitter(publicToPrivate, PublicToPrivateTranslation, outsPub, context)
		flow.CheckFatal(err)
		flow.CheckFatal(flow.SetStopper(pubTranslationOut[dirDROP]))

		// Initialize public KNI interface if requested
		if pp.PublicPort.KNIName != "" {
			pubKNI, err = flow.CreateKniDevice(pp.PublicPort.Index, pp.PublicPort.KNIName)
			flow.CheckFatal(err)
			flow.CheckFatal(flow.SetSenderKNI(pubTranslationOut[dirKNI], pubKNI))
			fromPubKNI = flow.SetReceiverKNI(pubKNI)
		}

		// Initialize private to public flow
		privateToPublic, err := flow.SetReceiver(pp.PrivatePort.Index)
		flow.CheckFatal(err)
		if pp.PrivatePort.KNIName != "" {
			outsPriv = 3
		}
		privTranslationOut, err := flow.SetSplitter(privateToPublic, PrivateToPublicTranslation, outsPriv, context)
		flow.CheckFatal(err)
		flow.CheckFatal(flow.SetStopper(privTranslationOut[dirDROP]))

		// Initialize private KNI interface if requested
		if pp.PrivatePort.KNIName != "" {
			privKNI, err = flow.CreateKniDevice(pp.PrivatePort.Index, pp.PrivatePort.KNIName)
			flow.CheckFatal(err)
			flow.CheckFatal(flow.SetSenderKNI(privTranslationOut[dirKNI], privKNI))
			fromPrivKNI = flow.SetReceiverKNI(privKNI)
		}

		// Merge traffic coming from public KNI with translated
		// traffic from private side
		if fromPubKNI != nil {
			toPub, err = flow.SetMerger(fromPubKNI, privTranslationOut[dirSEND])
			flow.CheckFatal(err)
		} else {
			toPub = privTranslationOut[dirSEND]
		}

		// Merge traffic coming from private KNI with translated
		// traffic from public side
		if fromPrivKNI != nil {
			toPriv, err = flow.SetMerger(fromPrivKNI, pubTranslationOut[dirSEND])
			flow.CheckFatal(err)
		} else {
			toPriv = pubTranslationOut[dirSEND]
		}

		// Set senders to output packets
		err = flow.SetSender(toPriv, pp.PrivatePort.Index)
		flow.CheckFatal(err)
		err = flow.SetSender(toPub, pp.PublicPort.Index)
		flow.CheckFatal(err)
	}
}

func CheckHWOffloading() bool {
	ports := []uint16{}

	for i := range Natconfig.PortPairs {
		pp := &Natconfig.PortPairs[i]
		ports = append(ports, pp.PublicPort.Index, pp.PrivatePort.Index)
	}

	return flow.CheckHWCapability(flow.HWTXChecksumCapability, ports)
}
