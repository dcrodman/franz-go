package kgo

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// Offset is a message offset in a partition.
type Offset struct {
	at           int64
	relative     int64
	epoch        int32
	currentEpoch int32 // set by us when mapping offsets to brokers
}

// NewOffsetcreates and returns an offset to use in AssignPartitions.
//
// The default offset begins at the end.
func NewOffset() Offset {
	return Offset{
		at:    -1,
		epoch: -1,
	}
}

// AtStart returns a copy of the calling offset, changing the returned offset
// to begin at the beginning of a partition.
func (o Offset) AtStart() Offset {
	o.at = -2
	return o
}

// AtEnd returns a copy of the calling offset, changing the returned offset to
// begin at the end of a partition.
func (o Offset) AtEnd() Offset {
	o.at = -1
	return o
}

// Relative returns a copy of the calling offset, changing the returned offset
// to be n relative to what it currently is. If the offset is beginning at the
// end, Relative(-100) will begin 100 before the end.
func (o Offset) Relative(n int64) Offset {
	o.relative = n
	return o
}

// WithEpoch returns a copy of the calling offset, changing the returned offset
// to use the given epoch. This epoch is used for truncation detection; the
// default of -1 implies no truncation detection.
func (o Offset) WithEpoch(e int32) Offset {
	if e < 0 {
		e = -1
	}
	o.epoch = e
	return o
}

// At returns a copy of the calling offset, changing the returned offset to
// begin at exactly the requested offset.
//
// There are two potential special offsets to use: -2 allows for consuming at
// the start, and -1 allows for consuming at the end. These two offsets are
// equivalent to calling AtStart or AtEnd.
//
// If the offset is less than -2, the client bounds it to -2 to consume at the
// start.
func (o Offset) At(at int64) Offset {
	if at < -2 {
		at = -2
	}
	o.at = at
	return o
}

type consumerType uint8

const (
	consumerTypeUnset consumerType = iota
	consumerTypeDirect
	consumerTypeGroup
)

type consumer struct {
	cl *Client

	// mu guards this block specifically
	mu     sync.Mutex
	group  *groupConsumer
	direct *directConsumer
	typ    consumerType

	// sessionChangeMu is grabbed when a session is stopped and held through
	// when a session can be started again. The sole purpose is to block an
	// assignment change running concurrently with a metadata update.
	sessionChangeMu sync.Mutex

	session atomic.Value // *consumerSession

	usingCursors usedCursors

	sourcesReadyMu          sync.Mutex
	sourcesReadyCond        *sync.Cond
	sourcesReadyForDraining []*source
	fakeReadyForDraining    []Fetch

	// dead is set when the client closes; this being true means that any
	// Assign does nothing (aside from unassigning everything prior).
	dead bool
}

type usedCursors map[*cursor]struct{}

func (u *usedCursors) use(c *cursor) {
	if *u == nil {
		*u = make(map[*cursor]struct{})
	}
	(*u)[c] = struct{}{}
}

// unset, called under the consumer mu, transitions the group to the unset
// state, invalidating old assignments and leaving a group if it was in one.
func (c *consumer) unset() {
	c.assignPartitions(nil, assignInvalidateAll)
	if c.typ == consumerTypeGroup {
		c.group.leave()
	}
	c.typ = consumerTypeUnset
	c.direct = nil
	c.group = nil
}

// addSourceReadyForDraining tracks that a source needs its buffered fetch
// consumed.
func (c *consumer) addSourceReadyForDraining(source *source) {
	c.sourcesReadyMu.Lock()
	c.sourcesReadyForDraining = append(c.sourcesReadyForDraining, source)
	c.sourcesReadyMu.Unlock()
	c.sourcesReadyCond.Broadcast()
}

// addFakeReadyForDraining saves a fake fetch that has important partition
// errors--data loss or auth failures.
func (c *consumer) addFakeReadyForDraining(topic string, partition int32, err error) {
	c.sourcesReadyMu.Lock()
	c.fakeReadyForDraining = append(c.fakeReadyForDraining, Fetch{Topics: []FetchTopic{{
		Topic: topic,
		Partitions: []FetchPartition{{
			Partition: partition,
			Err:       err,
		}},
	}}})
	c.sourcesReadyMu.Unlock()
	c.sourcesReadyCond.Broadcast()
}

// PollFetches waits for fetches to be available, returning as soon as any
// broker returns a fetch. If the ctx quits, this function quits.
//
// It is important to check all partition errors in the returned fetches. If
// any partition has a fatal error and actually had no records, fake fetch will
// be injected with the error.
//
// It is invalid to call this multiple times concurrently.
func (cl *Client) PollFetches(ctx context.Context) Fetches {
	c := &cl.consumer

	var fetches Fetches
	fill := func() {
		c.sourcesReadyMu.Lock()
		defer c.sourcesReadyMu.Unlock()
		for _, ready := range c.sourcesReadyForDraining {
			fetches = append(fetches, ready.takeBuffered())
		}
		c.sourcesReadyForDraining = nil

		// Before returning, we want to update our uncommitted. If we
		// updated after, then we could end up with weird interactions
		// with group invalidations where we return a stale fetch after
		// committing in onRevoke.
		//
		// A blocking onRevoke commit, on finish, allows a new group
		// session to start. If we returned stale fetches that did not
		// have their uncommitted offset tracked, then we would allow
		// duplicates.
		//
		// We grab the consumer mu because a concurrent client close
		// could happen.
		c.mu.Lock()
		if c.typ == consumerTypeGroup && len(fetches) > 0 {
			c.group.updateUncommitted(fetches)
		}
		c.mu.Unlock()

		fetches = append(fetches, c.fakeReadyForDraining...)
		c.fakeReadyForDraining = nil
	}

	fill()
	if len(fetches) > 0 {
		return fetches
	}

	done := make(chan struct{})
	quit := false
	go func() {
		c.sourcesReadyMu.Lock()
		defer c.sourcesReadyMu.Unlock()
		defer close(done)

		for !quit && len(c.sourcesReadyForDraining) == 0 {
			c.sourcesReadyCond.Wait()
		}
	}()

	select {
	case <-ctx.Done():
		c.sourcesReadyMu.Lock()
		quit = true
		c.sourcesReadyMu.Unlock()
		c.sourcesReadyCond.Broadcast()
	case <-done:
	}

	fill()
	return fetches
}

// assignHow controls how assignPartitions operates.
type assignHow int8

const (
	// This option simply assigns new offsets, doing nothing with existing
	// offsets / active fetches / buffered fetches.
	assignWithoutInvalidating assignHow = iota

	// This option invalidates active fetches so they will not buffer and
	// drops all buffered fetches, and then continues to assign the new
	// assignments.
	assignInvalidateAll

	// This option does not assign, but instead invalidates any active
	// fetches for "assigned" (actually lost) partitions. This additionally
	// drops all buffered fetches, because they could contain partitions we
	// lost. Thus, with this option, the actual offset in the map is
	// meaningless / a dummy offset.
	assignInvalidateMatching

	// The counterpart to assignInvalidateMatching, assignSetMatching
	// resets all matching partitions to the specified offset / epoch.
	assignSetMatching
)

func (h assignHow) String() string {
	switch h {
	case assignWithoutInvalidating:
		return "assign without invalidating"
	case assignInvalidateAll:
		return "assign invalidate all"
	case assignInvalidateMatching:
		return "assign invalidate matching"
	case assignSetMatching:
		return "assign set matching"
	}
	return ""
}

// assignPartitions, called under the consumer's mu, is used to set new
// cursors or add to the existing cursors.
func (c *consumer) assignPartitions(assignments map[string]map[int32]Offset, how assignHow) {
	fmt.Println("assigning", assignments, "how", how)
	var session *consumerSession
	var loadOffsets listOrEpochLoads
	defer func() {
		if session == nil { // if nil, we stopped the session
			session = c.startNewSession()
		}
		loadOffsets.loadWithSessionNow(session)
	}()

	if how == assignWithoutInvalidating {
		session = c.guardSessionChange()
		defer c.unguardSessionChange()

	} else {
		loadOffsets = c.stopSession()

		// First, over all cursors currently in use, we unset them or set them
		// directly as appropriate. Anything we do not unset, we keep.

		var keep usedCursors
		for usedCursor := range c.usingCursors {
			shouldKeep := true
			if how == assignInvalidateAll {
				usedCursor.unset()
				shouldKeep = false
			} else { // invalidateMatching or setMatching
				if assignTopic, ok := assignments[usedCursor.topic]; ok {
					if assignPart, ok := assignTopic[usedCursor.partition]; ok {
						if how == assignInvalidateMatching {
							usedCursor.unset()
							shouldKeep = false
						} else { // how == assignSetMatching
							fmt.Println("setting in assign", assignPart.at, assignPart.epoch)
							usedCursor.setOffset(cursorOffset{
								offset:            assignPart.at,
								lastConsumedEpoch: assignPart.epoch,
							})
						}
					}
				}
			}
			if shouldKeep {
				keep.use(usedCursor)
			}
		}
		c.usingCursors = keep

		// For any partition that was listing offsets or loading
		// epochs, we want to ensure that if we are keeping those
		// partitions, we re-start the list/load.
		//
		// Note that we do not need to unset cursors here; anything
		// that actually resulted in a cursor is forever tracked in
		// usedCursors. We only do not have used cursors if an
		// assignment went straight to listing / epoch loading, and
		// that list/epoch never finished.
		if how == assignInvalidateAll {
			loadOffsets = listOrEpochLoads{}
		} else {
			loadOffsets.filter(func(t string, p int32) bool {
				var wasLoading bool
				if assignTopic, ok := assignments[t]; ok {
					if _, ok := assignTopic[p]; ok {
						wasLoading = true
					}
				}
				return how == assignInvalidateMatching && !wasLoading ||
					how == assignSetMatching && wasLoading
			})
		}
	}

	// This assignment could contain nothing (for the purposes of
	// invalidating active fetches), so we only do this if needed.
	if len(assignments) == 0 || how == assignInvalidateMatching || how == assignSetMatching {
		return
	}

	c.cl.cfg.logger.Log(LogLevelDebug, "assign requires loading offsets")

	clientTopics := c.cl.loadTopics()
	for topic, partitions := range assignments {

		topicParts := clientTopics[topic].load() // must be non-nil, which is ensured in Assign<> or in metadata when consuming as regex

		for partition, offset := range partitions {
			// First, if the request is exact, get rid of the relative
			// portion. We are modifying a copy of the offset, i.e. we
			// are appropriately not modfying 'assignments' itself.
			if offset.at >= 0 {
				offset.at = offset.at + offset.relative
				if offset.at < 0 {
					offset.at = 0
				}
				offset.relative = 0
			}

			// If we are requesting an exact offset with an epoch,
			// we do truncation detection and then use the offset.
			//
			// Otherwise, an epoch is specified without an exact
			// request which is useless for us, or a request is
			// specified without a known epoch.
			if offset.at >= 0 && offset.epoch >= 0 {
				fmt.Println("adding load type epoch in assign", offset)
				loadOffsets.addLoad(topic, partition, loadTypeEpoch, offsetLoad{
					replica: -1,
					Offset:  offset,
				})
				continue
			}

			// If an exact offset is specified and we have loaded
			// the partition, we use it. Without an epoch, if it is
			// out of bounds, we just reset appropriately.
			//
			// If an offset is unspecified or we have not loaded
			// the partition, we list offsets to find out what to
			// use.
			if offset.at >= 0 && partition >= 0 && partition < int32(len(topicParts.partitions)) {
				part := topicParts.partitions[partition]
				cursor := part.cursor
				fmt.Println("setting in assign below", offset.at, part.leaderEpoch)
				cursor.setOffset(cursorOffset{
					offset:            offset.at,
					lastConsumedEpoch: part.leaderEpoch,
				})
				cursor.allowUsable()
				c.usingCursors.use(cursor)
				continue
			}

			fmt.Println("adding load type list in assign", offset)
			loadOffsets.addLoad(topic, partition, loadTypeList, offsetLoad{
				replica: -1,
				Offset:  offset,
			})
		}
	}
}

func (c *consumer) doOnMetadataUpdate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	switch c.typ {
	case consumerTypeUnset:
		return
	case consumerTypeDirect:
		c.assignPartitions(c.direct.findNewAssignments(c.cl.loadTopics()), assignWithoutInvalidating)
	case consumerTypeGroup:
		c.group.findNewAssignments(c.cl.loadTopics())
	}

	go c.loadSession().doOnMetadataUpdate()
}

func (s *consumerSession) doOnMetadataUpdate() {
	if s == nil { // no session started yet
		return
	}

	s.listOrEpochMu.Lock()
	defer s.listOrEpochMu.Unlock()

	if s.listOrEpochMetaCh == nil {
		return // nothing waiting to load epochs / offsets
	}
	select {
	case s.listOrEpochMetaCh <- struct{}{}:
	default:
	}
}

// offsetLoad is effectively an Offset, but also includes a potential replica
// to directly use if a cursor had a preferred replica.
type offsetLoad struct {
	replica int32 // -1 means leader
	Offset
}

type offsetLoadMap map[string]map[int32]offsetLoad

func (o offsetLoadMap) errToLoaded(err error) []loadedOffset {
	var loaded []loadedOffset
	for t, ps := range o {
		for p, o := range ps {
			loaded = append(loaded, loadedOffset{
				topic:     t,
				partition: p,
				err:       err,
				request:   o,
			})
		}
	}
	return loaded
}

// Combines list and epoch loads into one type for simplicity.
type listOrEpochLoads struct {
	list  offsetLoadMap
	epoch offsetLoadMap
}

type listOrEpochLoadType uint8

const (
	loadTypeList listOrEpochLoadType = iota
	loadTypeEpoch
)

// adds an offset to be loaded, ensuring it exists only in the final loadType.
func (l *listOrEpochLoads) addLoad(t string, p int32, loadType listOrEpochLoadType, load offsetLoad) {
	l.removeLoad(t, p)
	dst := &l.list
	if loadType == loadTypeEpoch {
		dst = &l.epoch
	}

	if *dst == nil {
		*dst = make(offsetLoadMap)
	}
	ps := (*dst)[t]
	if ps == nil {
		ps = make(map[int32]offsetLoad)
		(*dst)[t] = ps
	}
	ps[p] = load
}

func (l *listOrEpochLoads) removeLoad(t string, p int32) {
	for _, m := range []*offsetLoadMap{
		&l.list,
		&l.epoch,
	} {
		if *m == nil {
			continue
		}
		ps := (*m)[t]
		if ps == nil {
			continue
		}
		delete(ps, p)
		if len(ps) == 0 {
			delete(*m, t)
		}
	}
}

func (l listOrEpochLoads) each(fn func(string, int32)) {
	for _, m := range []offsetLoadMap{
		l.list,
		l.epoch,
	} {
		for topic, partitions := range m {
			for partition := range partitions {
				fn(topic, partition)
			}
		}
	}
}

func (l *listOrEpochLoads) filter(keep func(string, int32) bool) {
	for _, m := range []offsetLoadMap{
		l.list,
		l.epoch,
	} {
		for t, ps := range m {
			for p := range ps {
				if !keep(t, p) {
					delete(ps, p)
					if len(ps) == 0 {
						delete(m, t)
					}
				}
			}
		}
	}
}

// Merges loads into the caller; used to coalesce loads while a metadata update
// is happening (see the only use below).
func (dst *listOrEpochLoads) mergeFrom(src listOrEpochLoads) {
	for _, srcs := range []struct {
		m        offsetLoadMap
		loadType listOrEpochLoadType
	}{
		{src.list, loadTypeList},
		{src.epoch, loadTypeEpoch},
	} {
		for t, ps := range srcs.m {
			for p, load := range ps {
				dst.addLoad(t, p, srcs.loadType, load)
			}
		}
	}
}

func (l listOrEpochLoads) isEmpty() bool { return len(l.list) == 0 && len(l.epoch) == 0 }

func (l listOrEpochLoads) loadWithSession(s *consumerSession) {
	if !l.isEmpty() {
		s.incWorker()
		go s.listOrEpoch(l, false)
	}
}

func (l listOrEpochLoads) loadWithSessionNow(s *consumerSession) {
	if !l.isEmpty() {
		s.incWorker()
		go s.listOrEpoch(l, true)
	}
}

// A consumer session is responsible for an era of fetching records for a set
// of cursors. The set can be added to without killing an active session, but
// it cannot be removed from. Removing any cursor from being consumed kills the
// current consumer session and begins a new one.
type consumerSession struct {
	c *consumer

	ctx    context.Context
	cancel func()

	// Workers signify the number of fetch and list / epoch goroutines that
	// are currently running within the context of this consumer session.
	// Stopping a session only returns once workers hits zero.
	workersMu   sync.Mutex
	workersCond *sync.Cond
	workers     int

	listOrEpochMu           sync.Mutex
	listOrEpochLoadsWaiting listOrEpochLoads
	listOrEpochMetaCh       chan struct{} // non-nil if Loads is non-nil, signalled on meta update
	listOrEpochLoadsLoading listOrEpochLoads
}

func (c *consumer) newConsumerSession() *consumerSession {
	ctx, cancel := context.WithCancel(c.cl.ctx)
	session := &consumerSession{
		c: c,

		ctx:    ctx,
		cancel: cancel,
	}
	session.workersCond = sync.NewCond(&session.workersMu)
	return session
}

func (c *consumerSession) incWorker() {
	c.workersMu.Lock()
	defer c.workersMu.Unlock()
	c.workers++
}

func (c *consumerSession) decWorker() {
	c.workersMu.Lock()
	defer c.workersMu.Unlock()
	c.workers--
	if c.workers == 0 {
		c.workersCond.Broadcast()
	}
}

// noConsumerSession exists because we cannot store nil into an atomic.Value.
var noConsumerSession = new(consumerSession)

func (c *consumer) loadSession() *consumerSession {
	if session := c.session.Load(); session != nil {
		return session.(*consumerSession)
	}
	return noConsumerSession
}

// Guards against a session being stopped, and must be paired with an unguard.
// This returns a new session if there was no session.
//
// The purpose of this function is when performing additive-only changes to an
// existing session, because additive-only changes can avoid killing a running
// session.
func (c *consumer) guardSessionChange() *consumerSession {
	c.sessionChangeMu.Lock()

	session := c.loadSession()
	if session == noConsumerSession {
		// If there is no session, we simply store one. This is fine;
		// sources will be able to begin a fetch loop, but they will
		// have no cursors to consume yet.
		session = c.newConsumerSession()
		c.session.Store(session)
	}

	return session
}
func (c *consumer) unguardSessionChange() {
	c.sessionChangeMu.Unlock()
}

// Stops an active consumer session if there is one, and does not return until
// all fetching, listing, offset for leader epoching is complete. This
// invalidates any buffered fetches for the previous session and returns any
// partitions that were listing offsets or loading epochs.
func (c *consumer) stopSession() listOrEpochLoads {
	c.sessionChangeMu.Lock()

	session := c.loadSession()

	if session == noConsumerSession {
		return listOrEpochLoads{} // we had no session
	}

	// Before storing noConsumerSession, cancel our old. This pairs
	// with the reverse ordering in source, which checks noConsumerSession
	// then checks the session context.
	session.cancel()

	// At this point, any in progress fetches, offset lists, or epoch loads
	// will quickly die.

	c.session.Store(noConsumerSession)

	// At this point, no source can be started, because the session is
	// noConsumerSession.

	session.workersMu.Lock()
	for session.workers > 0 {
		session.workersCond.Wait()
	}
	session.workersMu.Unlock()

	// At this point, all fetches, lists, and loads are dead.

	c.cl.sinksAndSourcesMu.Lock()
	for _, sns := range c.cl.sinksAndSources {
		sns.source.session.reset()
	}
	c.cl.sinksAndSourcesMu.Unlock()

	// At this point, if we begin fetching anew, then the sources will not
	// be using stale sessions.

	c.sourcesReadyMu.Lock()
	defer c.sourcesReadyMu.Unlock()
	for _, ready := range c.sourcesReadyForDraining {
		ready.discardBuffered()
	}
	c.sourcesReadyForDraining = nil

	// At this point, we have invalidated any buffered data from the prior
	// session. We leave any fake things that were ready so that the user
	// can act on errors. The session is dead.

	session.listOrEpochLoadsWaiting.mergeFrom(session.listOrEpochLoadsLoading)
	return session.listOrEpochLoadsWaiting
}

// Starts a new consumer session, allowing fetches to happen.
func (c *consumer) startNewSession() *consumerSession {
	session := c.newConsumerSession()
	c.session.Store(session)

	// At this point, sources can start consuming.

	c.sessionChangeMu.Unlock()

	c.cl.sinksAndSourcesMu.Lock()
	for _, sns := range c.cl.sinksAndSources {
		sns.source.maybeConsume()
	}
	c.cl.sinksAndSourcesMu.Unlock()

	// At this point, any source that was not consuming becauase it saw the
	// session was stopped has been notified to potentially start consuming
	// again. The session is alive.

	return session
}

// This function is responsible for issuing ListOffsets or
// OffsetForLeaderEpoch. These requests's responses  are only handled within
// the context of a consumer session.
func (s *consumerSession) listOrEpoch(waiting listOrEpochLoads, immediate bool) {
	defer s.decWorker()

	if immediate {
		s.c.cl.triggerUpdateMetadataNow()
	} else {
		s.c.cl.triggerUpdateMetadata()
	}

	s.listOrEpochMu.Lock() // collapse any listOrEpochs that occur during meta update into one
	if !s.listOrEpochLoadsWaiting.isEmpty() {
		s.listOrEpochLoadsWaiting.mergeFrom(waiting)
		s.listOrEpochMu.Unlock()
		return
	}
	s.listOrEpochLoadsWaiting = waiting
	s.listOrEpochMetaCh = make(chan struct{}, 1)
	s.listOrEpochMu.Unlock()

	select {
	case <-s.ctx.Done():
		return
	case <-s.listOrEpochMetaCh:
	}

	s.listOrEpochMu.Lock()
	loading := s.listOrEpochLoadsWaiting
	s.listOrEpochLoadsLoading.mergeFrom(loading)
	s.listOrEpochLoadsWaiting = listOrEpochLoads{}
	s.listOrEpochMetaCh = nil
	s.listOrEpochMu.Unlock()

	brokerLoads := s.mapLoadsToBrokers(loading)

	results := make(chan loadedOffsets, 2*len(brokerLoads)) // each broker can receive up to two requests

	var issued, received int
	for broker, brokerLoad := range brokerLoads {
		s.c.cl.cfg.logger.Log(LogLevelDebug, "offsets to load broker", "broker", broker.meta.NodeID, "load", brokerLoad)
		if len(brokerLoad.list) > 0 {
			issued++
			go s.c.cl.listOffsetsForBrokerLoad(s.ctx, broker, brokerLoad.list, results)
		}
		if len(brokerLoad.epoch) > 0 {
			issued++
			go s.c.cl.loadEpochsForBrokerLoad(s.ctx, broker, brokerLoad.epoch, results)
		}
	}

	for received != issued {
		select {
		case <-s.ctx.Done():
			// If we return early, our session was canceled. We do
			// not move loading list or epoch loads back to
			// waiting; the session stopping manages that.
			return
		case loaded := <-results:
			received++
			s.handleListOrEpochResults(loaded)
		}
	}
}

// Called within a consumer session, this function handles results from list
// offsets or epoch loads.
func (s *consumerSession) handleListOrEpochResults(loaded loadedOffsets) {
	var reloads listOrEpochLoads
	defer func() {
		// When we are done handling results, we have finished loading
		// all the topics and partitions. We remove them from tracking
		// in our session.
		s.listOrEpochMu.Lock()
		for _, load := range loaded.loaded {
			s.listOrEpochLoadsLoading.removeLoad(load.topic, load.partition)
		}
		s.listOrEpochMu.Unlock()

		reloads.loadWithSession(s)
	}()

	for _, load := range loaded.loaded {
		use := func() {
			load.cursor.setOffset(cursorOffset{
				offset:            load.offset,
				lastConsumedEpoch: load.leaderEpoch,
			})
			load.cursor.allowUsable()
			s.c.usingCursors.use(load.cursor)
		}

		switch load.err.(type) {
		case *ErrDataLoss:
			s.c.addFakeReadyForDraining(load.topic, load.partition, load.err) // signal we lost data, but set the cursor to what we can
			use()

		case nil:
			use()

		default: // from ErrorCode in a response
			if !kerr.IsRetriable(load.err) { // non-retriable response error; signal such in a response
				s.c.addFakeReadyForDraining(load.topic, load.partition, load.err)
				continue
			}
			reloads.addLoad(load.topic, load.partition, loaded.loadType, load.request)
		}
	}
}

// Splits the loads into per-broker loads, mapping each partition to the broker
// that leads that partition.
func (s *consumerSession) mapLoadsToBrokers(loads listOrEpochLoads) map[*broker]listOrEpochLoads {
	brokerLoads := make(map[*broker]listOrEpochLoads)

	s.c.cl.brokersMu.RLock() // hold mu so we can check if partition leaders exist
	defer s.c.cl.brokersMu.RUnlock()

	brokers := s.c.cl.brokers
	seed := brokers[unknownSeedID(0)] // must be non-nil

	topics := s.c.cl.loadTopics()

	for _, loads := range []struct {
		m        offsetLoadMap
		loadType listOrEpochLoadType
	}{
		{loads.list, loadTypeList},
		{loads.epoch, loadTypeEpoch},
	} {
		for topic, partitions := range loads.m {
			topicPartitions := topics[topic].load()
			for partition, offset := range partitions {

				// We default to the first seed broker if we have no loaded
				// the broker leader for this partition (we should have).
				// Worst case, we get an error for the partition and retry.
				broker := seed
				if partition >= 0 && partition < int32(len(topicPartitions.partitions)) {
					topicPartition := topicPartitions.partitions[partition]
					brokerID := topicPartition.leader
					if offset.replica != -1 {
						// If we are fetching from a follower, we can list
						// offsets against the follower itself. The replica
						// being non-negative signals that.
						brokerID = offset.replica
					}
					if tryBroker := brokers[brokerID]; tryBroker != nil {
						broker = tryBroker
					}
					offset.currentEpoch = topicPartition.leaderEpoch // ensure we set our latest epoch for the partition
				}

				brokerLoad := brokerLoads[broker]
				brokerLoad.addLoad(topic, partition, loads.loadType, offset)
				brokerLoads[broker] = brokerLoad
			}
		}
	}

	return brokerLoads
}

// The result of ListOffsets or OffsetForLeaderEpoch for an individual
// partition.
type loadedOffset struct {
	topic     string
	partition int32

	// The following three are potentially unset if the error is non-nil
	// and not ErrDataLoss; these are what we loaded.
	cursor      *cursor
	offset      int64
	leaderEpoch int32

	// Any error encountered for loading this partition, or for epoch
	// loading, potentially ErrDataLoss. If this error is not retriable, we
	// avoid reloading the offset and instead inject a fake partition for
	// PollFetches containing this error.
	err error

	// The original request.
	request offsetLoad
}

// The results of ListOffsets or OffsetForLeaderEpoch for an individual broker.
type loadedOffsets struct {
	loaded   []loadedOffset
	loadType listOrEpochLoadType
}

func (l *loadedOffsets) add(a loadedOffset) { l.loaded = append(l.loaded, a) }
func (l *loadedOffsets) addAll(as []loadedOffset) loadedOffsets {
	l.loaded = append(l.loaded, as...)
	return *l
}

func (cl *Client) listOffsetsForBrokerLoad(ctx context.Context, broker *broker, load offsetLoadMap, results chan<- loadedOffsets) {
	loaded := loadedOffsets{loadType: loadTypeList}

	kresp, err := broker.waitResp(ctx, load.buildListReq(cl.cfg.isolationLevel))
	if err != nil {
		results <- loaded.addAll(load.errToLoaded(err))
		return
	}

	resp := kresp.(*kmsg.ListOffsetsResponse)
	for _, rTopic := range resp.Topics {
		topic := rTopic.Topic
		loadParts, ok := load[topic]
		if !ok {
			continue // should not happen: kafka replied with something we did not ask for
		}

		for _, rPartition := range rTopic.Partitions {
			partition := rPartition.Partition
			loadPart, ok := loadParts[partition]
			if !ok {
				continue // should not happen: kafka replied with something we did not ask for
			}

			if err := kerr.ErrorForCode(rPartition.ErrorCode); err != nil {
				loaded.add(loadedOffset{
					topic:     topic,
					partition: partition,
					err:       err,
					request:   loadPart,
				})
				continue // partition err: handled in results
			}

			topicPartitions := cl.loadTopics()[topic].load()
			if partition < 0 || partition >= int32(len(topicPartitions.partitions)) {
				continue // should not happen: we have not seen this partition from a metadata response
			}
			topicPartition := topicPartitions.partitions[partition]

			delete(loadParts, partition)
			if len(loadParts) == 0 {
				delete(load, topic)
			}

			offset := rPartition.Offset + loadPart.relative
			if len(rPartition.OldStyleOffsets) > 0 { // if we have any, we used list offsets v0
				offset = rPartition.OldStyleOffsets[0] + loadPart.relative
				fmt.Println("USING OLD STYLE OFFSETS", rPartition.OldStyleOffsets, offset)
			}
			if loadPart.at >= 0 {
				offset = loadPart.at + loadPart.relative // we obey exact requests, even if they end up past the end
			}
			if offset < 0 {
				offset = 0
			}

			loaded.add(loadedOffset{
				topic:       topic,
				partition:   partition,
				cursor:      topicPartition.cursor,
				offset:      offset,
				leaderEpoch: rPartition.LeaderEpoch,
				request:     loadPart,
			})
		}
	}

	results <- loaded.addAll(load.errToLoaded(kerr.UnknownTopicOrPartition))
}

func (cl *Client) loadEpochsForBrokerLoad(ctx context.Context, broker *broker, load offsetLoadMap, results chan<- loadedOffsets) {
	loaded := loadedOffsets{loadType: loadTypeEpoch}

	kresp, err := broker.waitResp(ctx, load.buildEpochReq())
	if err != nil {
		results <- loaded.addAll(load.errToLoaded(err))
		return
	}

	// If the version is < 2, we are speaking to an old broker. We should
	// not have an old version, but we could have spoken to a new broker
	// first then an old broker in the middle of a broker roll. For now, we
	// will just loop retrying until the broker is upgraded.

	resp := kresp.(*kmsg.OffsetForLeaderEpochResponse)
	for _, rTopic := range resp.Topics {
		topic := rTopic.Topic
		loadParts, ok := load[topic]
		if !ok {
			continue // should not happen: kafka replied with something we did not ask for
		}

		for _, rPartition := range rTopic.Partitions {
			partition := rPartition.Partition
			loadPart, ok := loadParts[partition]
			if !ok {
				continue // should not happen: kafka replied with something we did not ask for
			}

			if err := kerr.ErrorForCode(rPartition.ErrorCode); err != nil {
				loaded.add(loadedOffset{
					topic:     topic,
					partition: partition,
					err:       err,
					request:   loadPart,
				})
				continue // partition err: handled in results
			}

			topicPartitions := cl.loadTopics()[topic].load()
			if partition < 0 || partition >= int32(len(topicPartitions.partitions)) {
				continue // should not happen: we have not seen this partition from a metadata response
			}
			topicPartition := topicPartitions.partitions[partition]

			delete(loadParts, partition)
			if len(loadParts) == 0 {
				delete(load, topic)
			}

			offset := loadPart.at
			var err error
			if rPartition.EndOffset < offset {
				offset = rPartition.EndOffset
				err = &ErrDataLoss{topic, partition, offset, rPartition.EndOffset}
			}

			loaded.add(loadedOffset{
				topic:       topic,
				partition:   partition,
				cursor:      topicPartition.cursor,
				offset:      offset,
				leaderEpoch: rPartition.LeaderEpoch,
				err:         err,
				request:     loadPart,
			})
		}
	}

	results <- loaded.addAll(load.errToLoaded(kerr.UnknownTopicOrPartition))
}

func (o offsetLoadMap) buildListReq(isolationLevel int8) *kmsg.ListOffsetsRequest {
	req := &kmsg.ListOffsetsRequest{
		ReplicaID:      -1,
		IsolationLevel: isolationLevel,
		Topics:         make([]kmsg.ListOffsetsRequestTopic, 0, len(o)),
	}
	for topic, partitions := range o {
		parts := make([]kmsg.ListOffsetsRequestTopicPartition, 0, len(partitions))
		for partition, offset := range partitions {
			// If this partition is using an exact offset request,
			// then we are listing for a partition that was not yet
			// loaded by the client (due to metadata). We use -1
			// just to ensure the partition is loaded.
			timestamp := offset.at
			if timestamp >= 0 {
				timestamp = -1
			}
			fmt.Println("list offsets partition", partition, "timestamp", offset.at)
			parts = append(parts, kmsg.ListOffsetsRequestTopicPartition{
				Partition:          partition,
				CurrentLeaderEpoch: offset.currentEpoch, // KIP-320
				Timestamp:          offset.at,
				MaxNumOffsets:      1,
			})
		}
		req.Topics = append(req.Topics, kmsg.ListOffsetsRequestTopic{
			Topic:      topic,
			Partitions: parts,
		})
	}
	return req
}

func (o offsetLoadMap) buildEpochReq() *kmsg.OffsetForLeaderEpochRequest {
	req := &kmsg.OffsetForLeaderEpochRequest{
		ReplicaID: -1,
		Topics:    make([]kmsg.OffsetForLeaderEpochRequestTopic, 0, len(o)),
	}
	for topic, partitions := range o {
		parts := make([]kmsg.OffsetForLeaderEpochRequestTopicPartition, 0, len(partitions))
		for partition, offset := range partitions {
			parts = append(parts, kmsg.OffsetForLeaderEpochRequestTopicPartition{
				Partition:          partition,
				CurrentLeaderEpoch: offset.currentEpoch,
				LeaderEpoch:        offset.epoch,
			})
		}
		req.Topics = append(req.Topics, kmsg.OffsetForLeaderEpochRequestTopic{
			Topic:      topic,
			Partitions: parts,
		})
	}
	return req
}
