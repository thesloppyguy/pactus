package network

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	lp2p "github.com/libp2p/go-libp2p"
	lp2pps "github.com/libp2p/go-libp2p-pubsub"
	lp2pcore "github.com/libp2p/go-libp2p/core"
	lp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	lp2phost "github.com/libp2p/go-libp2p/core/host"
	lp2ppeer "github.com/libp2p/go-libp2p/core/peer"
	lp2prcmgr "github.com/libp2p/go-libp2p/p2p/host/resource-manager"
	lp2pconnmgr "github.com/libp2p/go-libp2p/p2p/net/connmgr"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/pactus-project/pactus/util"
	"github.com/pactus-project/pactus/util/logger"
	"github.com/pactus-project/pactus/version"
	"github.com/prometheus/client_golang/prometheus"
)

var _ Network = &network{}

type network struct {
	// TODO: Keeping context inside struct is bad practice:
	// Read more here: https://go.dev/blog/context-and-structs
	// We should remove it from here and pass it as first argument of functions
	// Adding these linter later:  contextcheck and containedctx
	ctx            context.Context
	cancel         func()
	name           string
	config         *Config
	host           lp2phost.Host
	mdns           *mdnsService
	dht            *dhtService
	peerMgr        *peerMgr
	stream         *streamService
	gossip         *gossipService
	notifee        *NotifeeService
	generalTopic   *lp2pps.Topic
	consensusTopic *lp2pps.Topic
	eventChannel   chan Event
	logger         *logger.SubLogger
}

func loadOrCreateKey(path string) (lp2pcrypto.PrivKey, error) {
	if util.PathExists(path) {
		h, err := util.ReadFile(path)
		if err != nil {
			return nil, err
		}
		bs, err := hex.DecodeString(string(h))
		if err != nil {
			return nil, err
		}
		key, err := lp2pcrypto.UnmarshalPrivateKey(bs)
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	key, _, err := lp2pcrypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to generate private key")
	}
	bs, err := lp2pcrypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, err
	}
	h := hex.EncodeToString(bs)
	err = util.WriteFile(path, []byte(h))
	if err != nil {
		return nil, err
	}
	return key, nil
}

func NewNetwork(networkName string, conf *Config) (Network, error) {
	return newNetwork(networkName, conf, []lp2p.Option{})
}

func newNetwork(networkName string, conf *Config, opts []lp2p.Option) (*network, error) {
	networkKey, err := loadOrCreateKey(conf.NetworkKey)
	if err != nil {
		return nil, LibP2PError{Err: err}
	}

	if conf.EnableMetrics {
		lp2prcmgr.MustRegisterWith(prometheus.DefaultRegisterer)
	}

	str, err := lp2prcmgr.NewStatsTraceReporter()
	if err != nil {
		return nil, LibP2PError{Err: err}
	}

	//
	// This is crazy!
	// Do they even know how to configure it properly?!
	//
	maxConns := conf.MaxConns
	minConns := conf.MinConns
	limit := lp2prcmgr.DefaultLimits
	limit.SystemBaseLimit.ConnsInbound = logScale(maxConns)
	limit.SystemBaseLimit.Conns = logScale(2 * maxConns)
	limit.SystemBaseLimit.StreamsInbound = logScale(maxConns)
	limit.SystemBaseLimit.Streams = logScale(2 * maxConns)

	limit.ServiceLimitIncrease.ConnsInbound = logScale(minConns)
	limit.ServiceLimitIncrease.Conns = logScale(2 * minConns)
	limit.ServiceLimitIncrease.StreamsInbound = logScale(minConns)
	limit.ServiceLimitIncrease.Streams = logScale(2 * minConns)

	limit.TransientBaseLimit.ConnsInbound = logScale(maxConns / 2)
	limit.TransientBaseLimit.Conns = logScale(2 * maxConns / 2)
	limit.TransientBaseLimit.StreamsInbound = logScale(maxConns / 2)
	limit.TransientBaseLimit.Streams = logScale(2 * maxConns / 2)

	limit.TransientLimitIncrease.ConnsInbound = logScale(minConns / 2)
	limit.TransientLimitIncrease.Conns = logScale(2 * minConns / 2)
	limit.TransientLimitIncrease.StreamsInbound = logScale(minConns / 2)
	limit.TransientLimitIncrease.Streams = logScale(2 * minConns / 2)

	resMgr, err := lp2prcmgr.NewResourceManager(
		lp2prcmgr.NewFixedLimiter(limit.AutoScale()),
		lp2prcmgr.WithTraceReporter(str),
	)
	if err != nil {
		return nil, LibP2PError{Err: err}
	}

	connMgr, err := lp2pconnmgr.NewConnManager(
		conf.MinConns, // Low Watermark
		conf.MaxConns, // High Watermark
		lp2pconnmgr.WithGracePeriod(time.Minute),
	)
	if err != nil {
		return nil, LibP2PError{Err: err}
	}

	opts = append(opts,
		lp2p.Identity(networkKey),
		lp2p.ListenAddrStrings(conf.Listens...),
		lp2p.UserAgent(version.Agent()),
		lp2p.ResourceManager(resMgr),
		lp2p.ConnectionManager(connMgr),
	)

	if !conf.EnableMetrics {
		opts = append(opts, lp2p.DisableMetrics())
	}

	if conf.EnableNAT {
		opts = append(opts,
			lp2p.EnableNATService(),
			lp2p.NATPortMap(),
		)
	}

	relayAddrs := []ma.Multiaddr{}
	if conf.EnableRelay {
		for _, s := range conf.RelayAddrs {
			addr, err := ma.NewMultiaddr(s)
			if err != nil {
				return nil, LibP2PError{Err: err}
			}
			relayAddrs = append(relayAddrs, addr)
		}

		static, err := lp2ppeer.AddrInfosFromP2pAddrs(relayAddrs...)
		if err != nil {
			return nil, LibP2PError{Err: err}
		}

		opts = append(opts,
			lp2p.EnableRelay(),
			lp2p.EnableAutoRelayWithStaticRelays(static),
			lp2p.EnableHolePunching(),
		)
	} else {
		opts = append(opts,
			lp2p.DisableRelay(),
		)
	}

	host, err := lp2p.New(opts...)
	if err != nil {
		return nil, LibP2PError{Err: err}
	}

	ctx, cancel := context.WithCancel(context.Background())

	n := &network{
		ctx:          ctx,
		cancel:       cancel,
		name:         networkName,
		config:       conf,
		host:         host,
		eventChannel: make(chan Event, 100),
	}

	n.logger = logger.NewSubLogger("_network", n)

	if conf.EnableMdns {
		n.mdns = newMdnsService(ctx, n.host, n.logger)
	}

	kadProtocolID := lp2pcore.ProtocolID(fmt.Sprintf("/%s/gossip/v1", n.name))
	streamProtocolID := lp2pcore.ProtocolID(fmt.Sprintf("/%s/stream/v1", n.name))

	n.dht = newDHTService(n.ctx, n.host, kadProtocolID, conf, n.logger)
	n.peerMgr = newPeerMgr(ctx, host, n.dht.kademlia, conf, n.logger)
	n.stream = newStreamService(ctx, n.host, streamProtocolID, relayAddrs, n.eventChannel, n.logger)
	n.gossip = newGossipService(ctx, n.host, n.eventChannel, conf, n.logger)
	n.notifee = newNotifeeService(n.host, n.eventChannel, n.logger, streamProtocolID)

	n.host.Network().Notify(n.notifee)

	n.logger.Info("network setup", "id", n.host.ID(),
		"address", conf.Listens,
		"bootstrapper", conf.Bootstrapper)

	return n, nil
}

func (n *network) EventChannel() <-chan Event {
	return n.eventChannel
}

func (n *network) Start() error {
	if err := n.dht.Start(); err != nil {
		return LibP2PError{Err: err}
	}
	if n.mdns != nil {
		if err := n.mdns.Start(); err != nil {
			return LibP2PError{Err: err}
		}
	}
	n.gossip.Start()
	n.stream.Start()
	n.peerMgr.Start()

	n.logger.Info("network started", "addr", n.host.Addrs(), "id", n.host.ID())
	return nil
}

func (n *network) Stop() {
	n.cancel()

	if n.mdns != nil {
		n.mdns.Stop()
	}

	n.dht.Stop()
	n.gossip.Stop()
	n.stream.Stop()
	n.peerMgr.Stop()

	if err := n.host.Close(); err != nil {
		n.logger.Error("unable to close the network", "error", err)
	}
}

func (n *network) SelfID() lp2ppeer.ID {
	return n.host.ID()
}

func (n *network) SendTo(msg []byte, pid lp2pcore.PeerID) error {
	return n.stream.SendRequest(msg, pid)
}

func (n *network) Broadcast(msg []byte, topicID TopicID) error {
	n.logger.Trace("publishing new message", "topic", topicID)
	switch topicID {
	case TopicIDGeneral:
		if n.generalTopic == nil {
			return NotSubscribedError{TopicID: topicID}
		}
		return n.gossip.BroadcastMessage(msg, n.generalTopic)

	case TopicIDConsensus:
		if n.consensusTopic == nil {
			return NotSubscribedError{TopicID: topicID}
		}
		return n.gossip.BroadcastMessage(msg, n.consensusTopic)

	default:
		return InvalidTopicError{TopicID: topicID}
	}
}

func (n *network) JoinGeneralTopic() error {
	if n.generalTopic != nil {
		n.logger.Debug("already subscribed to general topic")
		return nil
	}
	topic, err := n.gossip.JoinTopic(n.generalTopicName())
	if err != nil {
		return err
	}
	n.generalTopic = topic
	return nil
}

func (n *network) JoinConsensusTopic() error {
	if n.consensusTopic != nil {
		n.logger.Debug("already subscribed to consensus topic")
		return nil
	}
	topic, err := n.gossip.JoinTopic(n.consensusTopicName())
	if err != nil {
		return err
	}
	n.consensusTopic = topic
	return nil
}

func (n *network) generalTopicName() string {
	return n.TopicName("general")
}

func (n *network) consensusTopicName() string {
	return n.TopicName("consensus")
}

func (n *network) TopicName(topic string) string {
	return fmt.Sprintf("/%s/topic/%s/v1", n.name, topic)
}

func (n *network) CloseConnection(pid lp2ppeer.ID) {
	if err := n.host.Network().ClosePeer(pid); err != nil {
		n.logger.Warn("unable to close connection", "peer", pid)
	}
}

func (n *network) String() string {
	return fmt.Sprintf("{%d}", n.NumConnectedPeers())
}

func (n *network) NumConnectedPeers() int {
	return len(n.host.Network().Peers())
}
