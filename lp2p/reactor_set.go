package lp2p

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cometbft/cometbft/config"
	"github.com/cometbft/cometbft/internal/autopool"
	"github.com/cometbft/cometbft/p2p"
	"github.com/libp2p/go-libp2p/core/protocol"
)

// reactorSet manages multiple reactors as a single entrypoint for Switch.
type reactorSet struct {
	switchRef *Switch

	reactors []reactorItem

	// [reactor_name => reactor_idx] mapping
	reactorNames map[string]int

	// [protocol_id => reactorProtocol] mapping
	protocols map[protocol.ID]reactorProtocol
}

// reactorItem p2p.Reactor wrapper
type reactorItem struct {
	p2p.Reactor
	name          string
	consumerQueue *autopool.Pool[pendingEnvelope]
	queue         *autopool.PriorityQueue
}

// reactorProtocol represents mapping between [reactor, protocol, comet's channel descriptor]
type reactorProtocol struct {
	reactorID  int
	descriptor *p2p.ChannelDescriptor
}

// pendingEnvelope is a wrapper around p2p.Envelope
type pendingEnvelope struct {
	p2p.Envelope
	messageType  string
	addedAt      time.Time
	payloadBytes int
}

func newReactorSet(switchRef *Switch) *reactorSet {
	return &reactorSet{
		switchRef: switchRef,

		reactors:     []reactorItem{},
		reactorNames: make(map[string]int),
		protocols:    make(map[protocol.ID]reactorProtocol),
	}
}

// Add adds a new reactor to the set
// NOTE: not goroutine safe. Uses only for initialization.
func (rs *reactorSet) Add(reactor p2p.Reactor, name string) error {
	nextID := len(rs.reactors)

	if _, ok := rs.reactorNames[name]; ok {
		return fmt.Errorf("reactor %q is already registered", name)
	}

	// ensure reactor queue config
	scaler, err := rs.newReactorScaler(name)
	if err != nil {
		return fmt.Errorf("failed to create reactor scaler: %w", err)
	}

	maxMessageBytes := 0

	// register channel descriptor to reactor & protocolID mapping
	for i := range reactor.GetChannels() {
		var (
			channelDescriptor = reactor.GetChannels()[i]
			protocolID        = ProtocolID(channelDescriptor.ID)
		)

		if _, ok := rs.protocols[protocolID]; ok {
			return fmt.Errorf("protocol %q is already registered", protocolID)
		}

		rs.protocols[protocolID] = reactorProtocol{
			reactorID:  nextID,
			descriptor: channelDescriptor,
		}
		maxMessageBytes = max(maxMessageBytes, channelDescriptor.RecvMessageCapacity)
	}

	consumerQueue, queue := rs.newReactorPriorityQueue(nextID, name, scaler, maxMessageBytes)

	rs.reactors = append(rs.reactors, reactorItem{
		Reactor:       reactor,
		name:          name,
		consumerQueue: consumerQueue,
		queue:         queue,
	})

	// add name to mapping
	rs.reactorNames[name] = nextID

	rs.switchRef.Logger.Info("Added reactor", "reactor", name, "scaler", scaler)

	return nil
}

// Start starts all reactors with their receive queues
func (rs *reactorSet) Start(perProtocolCallback func(protocol.ID)) error {
	for _, reactor := range rs.reactors {
		rs.switchRef.Logger.Info("Starting reactor", "reactor", reactor.name)
		reactor.SetSwitch(rs.switchRef)

		if err := reactor.Start(); err != nil {
			return fmt.Errorf("failed to start reactor %s: %w", reactor.name, err)
		}

		reactor.consumerQueue.Start()
	}

	for protocolID := range rs.protocols {
		perProtocolCallback(protocolID)
	}

	return nil
}

func (rs *reactorSet) Stop() {
	for _, reactor := range rs.reactors {
		reactor.consumerQueue.Stop()

		rs.switchRef.Logger.Info("Stopping reactor", "reactor", reactor.name)
		if err := reactor.Stop(); err != nil {
			rs.switchRef.Logger.Error("Failed to stop reactor", "name", reactor.name, "err", err)
		}
	}
}

func (rs *reactorSet) InitPeer(peer *Peer) {
	for _, reactor := range rs.reactors {
		reactor.InitPeer(peer)
	}
}

func (rs *reactorSet) AddPeer(peer *Peer) {
	for _, reactor := range rs.reactors {
		reactor.AddPeer(peer)
	}
}

func (rs *reactorSet) RemovePeer(peer *Peer, reason any) {
	for _, reactor := range rs.reactors {
		reactor.RemovePeer(peer, reason)
	}
}

func (rs *reactorSet) GetByName(name string) (p2p.Reactor, bool) {
	reactor, ok := rs.getByName(name)
	if !ok {
		return nil, false
	}
	return reactor.Reactor, true
}

func (rs *reactorSet) getByName(name string) (reactorItem, bool) {
	idx, ok := rs.reactorNames[name]
	if !ok {
		return reactorItem{}, false
	}

	return rs.reactors[idx], true
}

func (rs *reactorSet) getReactorWithProtocol(id protocol.ID) (reactorProtocol, reactorItem, error) {
	protocol, ok := rs.protocols[id]
	if !ok {
		return reactorProtocol{}, reactorItem{}, fmt.Errorf("protocol not found")
	}

	return protocol, rs.reactors[protocol.reactorID], nil
}

// Receive schedules receive operation for a reactor. How it works:
// 1) pendingEnvelope is added to a priority queue that is sorted by priority and arrival time (FIFO)
// 2) Then the system pipes this queue to a concurrent pool that auto scales based on the load
// 3) autopool picks this channel, receives the message and calls reactorSet.receiveQueued(pendingEnvelope)
//
// This setup allows to handle lots of incoming message in a timely manner. System ensures that
// - All messages are sorted by priority, most important are processed first
// - We can process as many concurrent messages as possible
// - In case of latency degradation, the system is downscale to preserve processing speed.
func (rs *reactorSet) Receive(reactorName, messageType string, envelope p2p.Envelope, priority, payloadBytes int) {
	idx, ok := rs.reactorNames[reactorName]
	if !ok {
		rs.switchRef.Logger.Error("Receive: reactor not found", "reactor", reactorName)
		return
	}

	reactor := rs.reactors[idx]

	labels := messageLabels(reactorName, messageType)

	// lp2p metrics
	rs.switchRef.metrics.MessagesReceived.With(labels...).Add(1)
	rs.switchRef.metrics.MessagesReactorInFlight.With(labels...).Add(1)
	now := time.Now()

	pq := pendingEnvelope{
		Envelope:     envelope,
		messageType:  messageType,
		addedAt:      now,
		payloadBytes: payloadBytes,
	}

	err := reactor.consumerQueue.PushPriority(pq, priority)
	if err != nil {
		if errors.Is(err, autopool.ErrQueueFull) {
			rs.recordDrop(labels)
			rs.switchRef.Logger.Debug("Reactor queue full, dropping message", "reactor", reactorName, "message_type", messageType)
		} else {
			rs.switchRef.metrics.MessagesReactorInFlight.With(labels...).Add(-1)
			rs.switchRef.Logger.Error("Failed to push envelope to priority queue", "reactor", reactorName, "err", err)
		}
		return
	}

	rs.switchRef.Logger.Debug(
		"Envelope pushed to priority queue",
		"reactor", reactorName,
		"message_type", messageType,
	)
}

func (rs *reactorSet) receiveQueued(reactorID int, e pendingEnvelope) {
	reactor := rs.reactors[reactorID]
	defer reactor.queue.Release(e)

	labels := messageLabels(reactor.name, e.messageType)

	// duration between p2p receive and piping inside the consumer queue
	pendingFor := time.Since(e.addedAt)
	rs.switchRef.metrics.MessagesReactorPendingDuration.With(labels...).Observe(pendingFor.Seconds())

	// log slow pending envelopes if:
	// - pending for more than 1s (but only 10% sampled with a "dummy" sampling)
	// - pending for more than 10s
	logSlow := (pendingFor > time.Second) &&
		(pendingFor > 10*time.Second || e.addedAt.UnixMilli()%10 == 0)

	if logSlow {
		rs.switchRef.Logger.Info(
			"Slow pending envelope detected",
			"reactor", reactor.name,
			"message_type", e.messageType,
			"pending_for", pendingFor.String(),
		)
	}

	now := time.Now()

	defer rs.recoverReceive(reactor.name)
	reactor.Receive(e.Envelope)

	timeTaken := time.Since(now)

	rs.switchRef.metrics.MessagesReactorInFlight.With(labels...).Add(-1)
	rs.switchRef.metrics.MessageReactorReceiveDuration.With(labels...).Observe(timeTaken.Seconds())
}

func (rs *reactorSet) recordDrop(labels []string) {
	rs.switchRef.metrics.MessagesReactorInFlight.With(labels...).Add(-1)
	rs.switchRef.metrics.MessagesReactorDropped.With(labels...).Add(1)
}

func messageLabels(reactorName, messageType string) []string {
	return []string{"reactor", reactorName, "message_type", messageType}
}

func (rs *reactorSet) recoverReceive(reactorName string) {
	if p := recover(); p != nil {
		err := fmt.Errorf("panic: %+v", p)
		rs.switchRef.Logger.Error("Panic during receive", "reactor", reactorName, "err", err)
	}
}

// newReactorPriorityQueue creates a consumer pool for reactor.Receive()
// It allows to dynamically adjust consumption concurrency based on the load,
// while maintaining the priority, order, and latency of messages.
func (rs *reactorSet) newReactorPriorityQueue(
	reactorID int,
	reactorName string,
	scaler *autopool.ThroughputLatencyScaler,
	maxMessageBytes int,
) (*autopool.Pool[pendingEnvelope], *autopool.PriorityQueue) {
	const (
		// variety of priorities (cometbft has up to 10)
		priorities = 10

		// capacity of the concurrent pool (messages in flight)
		// others will be queued in the priority queue first (FIFO)
		concurrentPoolCapacity = 512
	)

	// an override bound of nil means unset (old/partial config); non-nil
	// (including *0) is an explicit choice, e.g. an intentional unbounded reactor.
	scalerCfg := rs.switchRef.host.config.Scaler
	maxQueueSize, maxQueueBytes := scalerCfg.MaxQueueSize, scalerCfg.MaxQueueBytes
	if override, ok := findScalerOverride(scalerCfg.Overrides, reactorName); ok {
		if override.MaxQueueSize != nil {
			maxQueueSize = *override.MaxQueueSize
		}
		if override.MaxQueueBytes != nil {
			maxQueueBytes = *override.MaxQueueBytes
		}
	}

	if maxQueueBytes > 0 && maxQueueBytes < maxMessageBytes {
		rs.switchRef.Logger.Error(
			"max_queue_bytes is below the reactor's largest message; larger messages will always be dropped",
			"reactor", reactorName,
			"max_queue_bytes", maxQueueBytes,
			"max_message_bytes", maxMessageBytes,
		)
	}

	concurrencyCounter := rs.
		switchRef.metrics.MessageReactorQueueConcurrency.
		With("reactor", reactorName)

	priorityQueue := autopool.NewPriorityQueueWithMax(
		priorities,
		maxQueueSize,
		autopool.WithMaxBytes(maxQueueBytes, func(v any) int {
			e, _ := v.(pendingEnvelope)
			return e.payloadBytes
		}),
		autopool.WithOnEvict(func(v any) {
			e, ok := v.(pendingEnvelope)
			if !ok {
				return
			}
			rs.recordDrop(messageLabels(reactorName, e.messageType))
		}),
	)

	return autopool.New(
		scaler,
		func(e pendingEnvelope) {
			rs.receiveQueued(reactorID, e)
		},
		concurrentPoolCapacity,
		autopool.WithLogger[pendingEnvelope](rs.switchRef.Logger),
		autopool.WithOnScale[pendingEnvelope](func() {
			concurrencyCounter.Add(1)
		}),
		autopool.WithOnShrink[pendingEnvelope](func() {
			concurrencyCounter.Add(-1)
		}),
		autopool.WithPriorityQueue[pendingEnvelope](priorityQueue),
	), priorityQueue
}

func (rs *reactorSet) newReactorScaler(reactorName string) (*autopool.ThroughputLatencyScaler, error) {
	const (
		// how frequently to decide on scaling action
		scalerFrequency = 200 * time.Millisecond
		// use P90 for latency measurement
		latencyPercentile = 90.0
	)

	var (
		// we expect the config to be valid
		scalerConfig     = rs.switchRef.host.config.Scaler
		minWorkers       = scalerConfig.MinWorkers
		maxWorkers       = scalerConfig.MaxWorkers
		latencyThreshold = scalerConfig.ThresholdLatency
	)

	if override, ok := findScalerOverride(scalerConfig.Overrides, reactorName); ok {
		minWorkers = override.MinWorkers
		maxWorkers = override.MaxWorkers
		latencyThreshold = override.ThresholdLatency
	}

	return autopool.NewThroughputLatencyScaler(
		minWorkers,
		maxWorkers,
		latencyPercentile,
		latencyThreshold,
		scalerFrequency,
		rs.switchRef.Logger,
	), nil
}

// findScalerOverride returns the scaler override configured for reactorName
// (case-insensitive), if any.
func findScalerOverride(overrides []config.LibP2PScalerOverride, reactorName string) (config.LibP2PScalerOverride, bool) {
	for _, override := range overrides {
		if strings.EqualFold(override.Reactor, reactorName) {
			return override, true
		}
	}
	return config.LibP2PScalerOverride{}, false
}

func (rp *reactorProtocol) maxMessageSize() uint64 {
	return uint64(rp.descriptor.RecvMessageCapacity)
}
