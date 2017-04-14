package p2p

import (
	"bytes"
	"fmt"
	"math/rand"
	"reflect"
	"time"

	. "github.com/tendermint/go-common"
	wire "github.com/tendermint/go-wire"
)

const (
	PexChannel = byte(0x00)
	// period to ensure peers connected
	defaultEnsurePeersPeriod = 30 * time.Second
	minNumOutboundPeers      = 10
	maxPexMessageSize        = 1048576 // 1MB

	// maximum messages one peer can send to us during `msgCountByPeerFlushInterval`
	defaultMaxMsgCountByPeer    = 1000
	msgCountByPeerFlushInterval = 1 * time.Hour
)

// PEXReactor handles PEX (peer exchange) and ensures that an
// adequate number of peers are connected to the switch.
type PEXReactor struct {
	BaseReactor

	sw                *Switch
	book              *AddrBook
	ensurePeersPeriod time.Duration

	// tracks message count by peer, so we can prevent abuse
	msgCountByPeer    map[string]uint16
	maxMsgCountByPeer uint16
}

func NewPEXReactor(b *AddrBook) *PEXReactor {
	r := &PEXReactor{
		book:              b,
		ensurePeersPeriod: defaultEnsurePeersPeriod,
		msgCountByPeer:    make(map[string]uint16),
		maxMsgCountByPeer: defaultMaxMsgCountByPeer,
	}
	r.BaseReactor = *NewBaseReactor(log, "PEXReactor", r)
	return r
}

func (r *PEXReactor) OnStart() error {
	r.BaseReactor.OnStart()
	r.book.Start()
	go r.ensurePeersRoutine()
	go r.flushMsgCountByPeer()
	return nil
}

func (r *PEXReactor) OnStop() {
	r.BaseReactor.OnStop()
	r.book.Stop()
}

// GetChannels implements Reactor
func (r *PEXReactor) GetChannels() []*ChannelDescriptor {
	return []*ChannelDescriptor{
		&ChannelDescriptor{
			ID:                PexChannel,
			Priority:          1,
			SendQueueCapacity: 10,
		},
	}
}

// AddPeer implements Reactor by adding peer to the address book (if inbound)
// or by requesting more addresses (if outbound).
func (r *PEXReactor) AddPeer(p *Peer) {
	netAddr, err := NewNetAddressString(p.ListenAddr)
	if err != nil {
		// this should never happen
		log.Error("Error in AddPeer: invalid peer address", "addr", p.ListenAddr, "error", err)
		return
	}

	if p.IsOutbound() { // For outbound peers, the address is already in the books
		if r.book.NeedMoreAddrs() {
			r.RequestPEX(p)
		}
	} else { // For inbound connections, the peer is its own source
		addr := NewNetAddressString(p.ListenAddr)
		r.book.AddAddress(addr, addr)
	}
}

// RemovePeer implements Reactor
func (r *PEXReactor) RemovePeer(p *Peer, reason interface{}) {
	addr := NewNetAddressString(p.ListenAddr)
	// addr will be ejected from the book
	r.book.MarkBad(addr)
}

// Receive implements Reactor by handling incoming PEX messages.
func (r *PEXReactor) Receive(chID byte, src *Peer, msgBytes []byte) {
	srcAddr := src.Connection().RemoteAddress
	srcAddrStr := srcAddr.String()
	r.msgCountByPeer[srcAddrStr]++
	if r.ReachedMaxMsgCountForPeer(srcAddrStr) {
		log.Warn("Maximum number of messages reached for peer", "peer", srcAddrStr)
		// TODO remove src from peers?
		return
	}

	_, msg, err := DecodeMessage(msgBytes)
	if err != nil {
		log.Warn("Error decoding message", "error", err)
		return
	}
	log.Notice("Received message", "msg", msg)

	switch msg := msg.(type) {
	case *pexRequestMessage:
		// src requested some peers.
		r.SendAddrs(src, r.book.GetSelection())
	case *pexAddrsMessage:
		// We received some peer addresses from src.
		// (We don't want to get spammed with bad peers)
		for _, addr := range msg.Addrs {
			if addr != nil {
				r.book.AddAddress(addr, srcAddr)
			}
		}
	default:
		log.Warn(Fmt("Unknown message type %v", reflect.TypeOf(msg)))
	}
}

// RequestPEX asks peer for more addresses.
func (r *PEXReactor) RequestPEX(p *Peer) {
	p.Send(PexChannel, struct{ PexMessage }{&pexRequestMessage{}})
}

// SendAddrs sends addrs to the peer.
func (r *PEXReactor) SendAddrs(p *Peer, addrs []*NetAddress) {
	p.Send(PexChannel, struct{ PexMessage }{&pexAddrsMessage{Addrs: addrs}})
}

// SetEnsurePeersPeriod sets period to ensure peers connected.
func (r *PEXReactor) SetEnsurePeersPeriod(d time.Duration) {
	r.ensurePeersPeriod = d
}

// SetMaxMsgCountByPeer sets maximum messages one peer can send to us during 'msgCountByPeerFlushInterval'.
func (r *PEXReactor) SetMaxMsgCountByPeer(v uint16) {
	r.maxMsgCountByPeer = v
}

// ReachedMaxMsgCountForPeer returns true if we received too many
// messages from peer with address `addr`.
func (r *PEXReactor) ReachedMaxMsgCountForPeer(addr string) bool {
	return r.msgCountByPeer[addr] >= r.maxMsgCountByPeer
}

// Ensures that sufficient peers are connected. (continuous)
func (r *PEXReactor) ensurePeersRoutine() {
	// Randomize when routine starts
	ensurePeersPeriodMs := r.ensurePeersPeriod.Nanoseconds() / 1e6
	time.Sleep(time.Duration(rand.Int63n(ensurePeersPeriodMs)) * time.Millisecond)

	// fire once immediately.
	r.ensurePeers()

	// fire periodically
	ticker := time.NewTicker(r.ensurePeersPeriod)

	for {
		select {
		case <-ticker.C:
			r.ensurePeers()
		case <-r.Quit:
			ticker.Stop()
			return
		}
	}
}

// ensurePeers ensures that sufficient peers are connected. (once)
func (r *PEXReactor) ensurePeers() {
	numOutPeers, _, numDialing := r.Switch.NumPeers()
	numToDial := minNumOutboundPeers - (numOutPeers + numDialing)
	log.Info("Ensure peers", "numOutPeers", numOutPeers, "numDialing", numDialing, "numToDial", numToDial)
	if numToDial <= 0 {
		return
	}
	toDial := NewCMap()

	// Try to pick numToDial addresses to dial.
	// TODO: improve logic.
	for i := 0; i < numToDial; i++ {
		newBias := MinInt(numOutPeers, 8)*10 + 10
		var picked *NetAddress
		// Try to fetch a new peer 3 times.
		// This caps the maximum number of tries to 3 * numToDial.
		for j := 0; j < 3; j++ {
			try := r.book.PickAddress(newBias)
			if try == nil {
				break
			}
			alreadySelected := toDial.Has(try.IP.String())
			alreadyDialing := r.Switch.IsDialing(try)
			alreadyConnected := r.Switch.Peers().Has(try.IP.String())
			if alreadySelected || alreadyDialing || alreadyConnected {
				/*
					log.Info("Cannot dial address", "addr", try,
						"alreadySelected", alreadySelected,
						"alreadyDialing", alreadyDialing,
						"alreadyConnected", alreadyConnected)
				*/
				continue
			} else {
				log.Info("Will dial address", "addr", try)
				picked = try
				break
			}
		}
		if picked == nil {
			continue
		}
		toDial.Set(picked.IP.String(), picked)
	}

	// Dial picked addresses
	for _, item := range toDial.Values() {
		go func(picked *NetAddress) {
			_, err := r.Switch.DialPeerWithAddress(picked, false)
			if err != nil {
				r.book.MarkAttempt(picked)
			}
		}(item.(*NetAddress))
	}

	// If we need more addresses, pick a random peer and ask for more.
	if r.book.NeedMoreAddrs() {
		if peers := r.Switch.Peers().List(); len(peers) > 0 {
			i := rand.Int() % len(peers)
			peer := peers[i]
			log.Info("No addresses to dial. Sending pexRequest to random peer", "peer", peer)
			r.RequestPEX(peer)
		}
	}
}

func (r *PEXReactor) flushMsgCountByPeer() {
	ticker := time.NewTicker(msgCountByPeerFlushInterval)

	for {
		select {
		case <-ticker.C:
			r.msgCountByPeer = make(map[string]uint16)
		case <-r.Quit:
			ticker.Stop()
			return
		}
	}
}

//-----------------------------------------------------------------------------
// Messages

const (
	msgTypeRequest = byte(0x01)
	msgTypeAddrs   = byte(0x02)
)

type PexMessage interface{}

var _ = wire.RegisterInterface(
	struct{ PexMessage }{},
	wire.ConcreteType{&pexRequestMessage{}, msgTypeRequest},
	wire.ConcreteType{&pexAddrsMessage{}, msgTypeAddrs},
)

func DecodeMessage(bz []byte) (msgType byte, msg PexMessage, err error) {
	msgType = bz[0]
	n := new(int)
	r := bytes.NewReader(bz)
	msg = wire.ReadBinary(struct{ PexMessage }{}, r, maxPexMessageSize, n, &err).(struct{ PexMessage }).PexMessage
	return
}

/*
A pexRequestMessage requests additional peer addresses.
*/
type pexRequestMessage struct {
}

func (m *pexRequestMessage) String() string {
	return "[pexRequest]"
}

/*
A message with announced peer addresses.
*/
type pexAddrsMessage struct {
	Addrs []*NetAddress
}

func (m *pexAddrsMessage) String() string {
	return fmt.Sprintf("[pexAddrs %v]", m.Addrs)
}
