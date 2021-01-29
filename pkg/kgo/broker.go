package kgo

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kbin"
	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kmsg"
	"github.com/twmb/franz-go/pkg/sasl"
)

type promisedReq struct {
	ctx     context.Context
	req     kmsg.Request
	promise func(kmsg.Response, error)
	enqueue time.Time // used to calculate writeWait
}

type promisedResp struct {
	ctx    context.Context
	corrID int32

	readTimeout time.Duration

	// With flexible headers, we skip tags at the end of the response
	// header for now because they're currently unused. However, the
	// ApiVersions response uses v0 response header (no tags) even if the
	// response body has flexible versions. This is done in support of the
	// v0 fallback logic that allows for indexing into an exact offset.
	// Thus, for ApiVersions specifically, this is false even if the
	// request is flexible.
	//
	// As a side note, this note was not mentioned in KIP-482 which
	// introduced flexible versions, and was mentioned in passing in
	// KIP-511 which made ApiVersion flexible, so discovering what was
	// wrong was not too fun ("Note that ApiVersionsResponse is flexible
	// version but the response header is not flexible" is *it* in the
	// entire KIP.)
	//
	// To see the version pinning, look at the code generator function
	// generateHeaderVersion in
	// generator/src/main/java/org/apache/kafka/message/ApiMessageTypeGenerator.java
	flexibleHeader bool

	resp    kmsg.Response
	promise func(kmsg.Response, error)

	enqueue time.Time // used to calculate readWait
}

var unknownMetadata = BrokerMetadata{
	NodeID: -1,
}

// BrokerMetadata is metadata for a broker.
//
// This struct mirrors kmsg.MetadataResponseBroker.
type BrokerMetadata struct {
	// NodeID is the broker node ID.
	//
	// Seed brokers will have very negative IDs; kgo does not try to map
	// seed brokers to loaded brokers.
	NodeID int32

	// Port is the port of the broker.
	Port int32

	// Host is the hostname of the broker.
	Host string

	// Rack is an optional rack of the broker. It is invalid to modify this
	// field.
	//
	// Seed brokers will not have a rack.
	Rack *string

	_internal struct{} // allow us to add fields later
}

func (this BrokerMetadata) equals(other kmsg.MetadataResponseBroker) bool {
	return this.NodeID == other.NodeID &&
		this.Port == other.Port &&
		this.Host == other.Host &&
		(this.Rack == nil && other.Rack == nil ||
			this.Rack != nil && other.Rack != nil && *this.Rack == *other.Rack)
}

// broker manages the concept how a client would interact with a broker.
type broker struct {
	cl *Client

	addr string // net.JoinHostPort(meta.Host, meta.Port)
	meta BrokerMetadata

	// The cxn fields each manage a single tcp connection to one broker.
	// Each field is managed serially in handleReqs. This means that only
	// one write can happen at a time, regardless of which connection the
	// write goes to, but the write is expected to be fast whereas the wait
	// for the response is expected to be slow.
	//
	// Produce requests go to cxnProduce, fetch to cxnFetch, and all others
	// to cxnNormal.
	cxnNormal  *brokerCxn
	cxnProduce *brokerCxn
	cxnFetch   *brokerCxn

	// dieMu guards sending to reqs in case the broker has been
	// permanently stopped.
	dieMu sync.RWMutex
	// reqs manages incoming message requests.
	reqs chan promisedReq
	// dead is an atomic so a backed up reqs cannot block broker stoppage.
	dead int32
}

const unknownControllerID = -1

// broker IDs are all positive, but Kafka uses -1 to signify unknown
// controllers. To avoid issues where a client broker ID map knows of
// a -1 ID controller, we start unknown seeds at MinInt32.
func unknownSeedID(seedNum int) int32 {
	return int32(math.MinInt32 + seedNum)
}

func (cl *Client) newBroker(nodeID int32, host string, port int32, rack *string) *broker {
	br := &broker{
		cl: cl,

		addr: net.JoinHostPort(host, strconv.Itoa(int(port))),
		meta: BrokerMetadata{
			NodeID: nodeID,
			Host:   host,
			Port:   port,
			Rack:   rack,
		},

		reqs: make(chan promisedReq, 10),
	}
	go br.handleReqs()

	return br
}

// stopForever permanently disables this broker.
func (b *broker) stopForever() {
	if atomic.SwapInt32(&b.dead, 1) == 1 {
		return
	}

	// begin draining reqs before lock/unlocking to ensure nothing
	// sitting on the rlock will block our lock
	go func() {
		for pr := range b.reqs {
			pr.promise(nil, ErrBrokerDead)
		}
	}()

	b.dieMu.Lock()
	b.dieMu.Unlock()

	// after dieMu, nothing will be sent down reqs
	close(b.reqs)
}

// do issues a request to the broker, eventually calling the response
// once a the request either fails or is responded to (with failure or not).
//
// The promise will block broker processing.
func (b *broker) do(
	ctx context.Context,
	req kmsg.Request,
	promise func(kmsg.Response, error),
) {
	dead := false

	enqueue := time.Now()
	b.dieMu.RLock()
	if atomic.LoadInt32(&b.dead) == 1 {
		dead = true
	} else {
		b.reqs <- promisedReq{ctx, req, promise, enqueue}
	}
	b.dieMu.RUnlock()

	if dead {
		promise(nil, ErrBrokerDead)
	}
}

// waitResp runs a req, waits for the resp and returns the resp and err.
func (b *broker) waitResp(ctx context.Context, req kmsg.Request) (kmsg.Response, error) {
	var resp kmsg.Response
	var err error
	done := make(chan struct{})
	wait := func(kresp kmsg.Response, kerr error) {
		resp, err = kresp, kerr
		close(done)
	}
	b.do(ctx, req, wait)
	<-done
	return resp, err
}

// handleReqs manages the intake of message requests for a broker.
//
// This creates connections as appropriate, serializes the request, and sends
// awaiting responses with the request promise to be handled as appropriate.
//
// If any of these steps fail, the promise is called with the relevant error.
func (b *broker) handleReqs() {
	defer func() {
		b.cxnNormal.die()
		b.cxnProduce.die()
		b.cxnFetch.die()
	}()

	for pr := range b.reqs {
		req := pr.req
		cxn, err := b.loadConnection(pr.ctx, req.Key())
		if err != nil {
			pr.promise(nil, err)
			continue
		}

		if int(req.Key()) > len(cxn.versions[:]) ||
			b.cl.cfg.maxVersions != nil && !b.cl.cfg.maxVersions.HasKey(req.Key()) {
			pr.promise(nil, ErrUnknownRequestKey)
			continue
		}

		// If cxn.versions[0] is non-negative, then we loaded API
		// versions. If the version for this request is negative, we
		// know the broker cannot handle this request.
		if cxn.versions[0] >= 0 && cxn.versions[req.Key()] < 0 {
			pr.promise(nil, ErrBrokerTooOld)
			continue
		}

		ourMax := req.MaxVersion()
		if b.cl.cfg.maxVersions != nil {
			userMax, _ := b.cl.cfg.maxVersions.LookupMaxKeyVersion(req.Key()) // we validated HasKey above
			if userMax < ourMax {
				ourMax = userMax
			}
		}

		// If brokerMax is negative at this point, we have no api
		// versions because the client is pinned pre 0.10.0 and we
		// stick with our max.
		version := ourMax
		if brokerMax := cxn.versions[req.Key()]; brokerMax >= 0 && brokerMax < ourMax {
			version = brokerMax
		}

		// If the version now (after potential broker downgrading) is
		// lower than we desire, we fail the request for the broker is
		// too old.
		if b.cl.cfg.minVersions != nil {
			minVersion, minVersionExists := b.cl.cfg.minVersions.LookupMaxKeyVersion(req.Key())
			if minVersionExists && version < minVersion {
				pr.promise(nil, ErrBrokerTooOld)
				continue
			}
		}

		req.SetVersion(version) // always go for highest version

		if !cxn.expiry.IsZero() && time.Now().After(cxn.expiry) {
			// If we are after the reauth time, try to reauth. We
			// can only have an expiry if we went the authenticate
			// flow, so we know we are authenticating again.
			// For KIP-368.
			if err = cxn.sasl(); err != nil {
				pr.promise(nil, err)
				cxn.die()
				continue
			}
		}

		// Juuuust before we issue the request, we check if it was
		// canceled. We could have previously tried this request, which
		// then failed and retried due to the error being ErrConnDead.
		// Checking the context was canceled here ensures we do not
		// loop. We could be more precise with error tracking, though.
		select {
		case <-pr.ctx.Done():
			pr.promise(nil, pr.ctx.Err())
			continue
		default:
		}

		corrID, err := cxn.writeRequest(pr.ctx, pr.enqueue, req)

		if err != nil {
			pr.promise(nil, err)
			cxn.die()
			continue
		}

		rt, _ := cxn.cl.connTimeoutFn(req)

		cxn.waitResp(promisedResp{
			pr.ctx,
			corrID,
			rt,
			req.IsFlexible() && req.Key() != 18, // response header not flexible if ApiVersions; see promisedResp doc
			req.ResponseKind(),
			pr.promise,
			time.Now(),
		})
	}
}

// bufPool is used to reuse issued-request buffers across writes to brokers.
type bufPool struct{ p *sync.Pool }

func newBufPool() bufPool {
	return bufPool{
		p: &sync.Pool{New: func() interface{} { r := make([]byte, 1<<10); return &r }},
	}
}

func (p bufPool) get() []byte  { return (*p.p.Get().(*[]byte))[:0] }
func (p bufPool) put(b []byte) { p.p.Put(&b) }

// loadConection returns the broker's connection, creating it if necessary
// and returning an error of if that fails.
func (b *broker) loadConnection(ctx context.Context, reqKey int16) (*brokerCxn, error) {
	pcxn := &b.cxnNormal
	if reqKey == 0 {
		pcxn = &b.cxnProduce
	} else if reqKey == 1 {
		pcxn = &b.cxnFetch
	}

	if *pcxn != nil && atomic.LoadInt32(&(*pcxn).dead) == 0 {
		return *pcxn, nil
	}

	conn, err := b.connect(ctx)
	if err != nil {
		return nil, err
	}

	cxn := &brokerCxn{
		cl: b.cl,
		b:  b,

		addr:   b.addr,
		conn:   conn,
		deadCh: make(chan struct{}),
	}
	if err = cxn.init(); err != nil {
		b.cl.cfg.logger.Log(LogLevelDebug, "connection initialization failed", "addr", b.addr, "id", b.meta.NodeID, "err", err)
		cxn.closeConn()
		return nil, err
	}
	b.cl.cfg.logger.Log(LogLevelDebug, "connection initialized successfully", "addr", b.addr, "id", b.meta.NodeID)

	*pcxn = cxn
	return cxn, nil
}

// connect connects to the broker's addr, returning the new connection.
func (b *broker) connect(ctx context.Context) (net.Conn, error) {
	b.cl.cfg.logger.Log(LogLevelDebug, "opening connection to broker", "addr", b.addr, "id", b.meta.NodeID)
	start := time.Now()
	conn, err := b.cl.cfg.dialFn(ctx, "tcp", b.addr)
	since := time.Since(start)
	b.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(BrokerConnectHook); ok {
			h.OnConnect(b.meta, since, conn, err)
		}
	})
	if err != nil {
		b.cl.cfg.logger.Log(LogLevelWarn, "unable to open connection to broker", "addr", b.addr, "id", b.meta.NodeID, "err", err)
		if _, ok := err.(net.Error); ok {
			return nil, ErrNoDial
		}
		return nil, err
	} else {
		b.cl.cfg.logger.Log(LogLevelDebug, "connection opened to broker", "addr", b.addr, "id", b.meta.NodeID)
	}
	return conn, nil
}

// brokerCxn manages an actual connection to a Kafka broker. This is separate
// the broker struct to allow lazy connection (re)creation.
type brokerCxn struct {
	conn net.Conn

	cl *Client
	b  *broker

	addr     string
	versions [kmsg.MaxKey + 1]int16

	mechanism sasl.Mechanism
	expiry    time.Time

	throttleUntil int64 // atomic nanosec

	corrID int32

	// dieMu guards sending to resps in case the connection has died.
	dieMu sync.RWMutex
	// resps manages reading kafka responses.
	resps chan promisedResp
	// dead is an atomic so that a backed up resps cannot block cxn death.
	dead int32
	// closed in cloneConn; allows throttle waiting to quit
	deadCh chan struct{}
}

func (cxn *brokerCxn) init() error {
	for i := 0; i < len(cxn.versions[:]); i++ {
		cxn.versions[i] = -1
	}

	if cxn.b.cl.cfg.maxVersions == nil || cxn.b.cl.cfg.maxVersions.HasKey(18) {
		if err := cxn.requestAPIVersions(); err != nil {
			cxn.cl.cfg.logger.Log(LogLevelError, "unable to request api versions", "err", err)
			return err
		}
	}

	if err := cxn.sasl(); err != nil {
		cxn.cl.cfg.logger.Log(LogLevelError, "unable to initialize sasl", "err", err)
		return err
	}

	cxn.resps = make(chan promisedResp, 10)
	go cxn.handleResps()
	return nil
}

func (cxn *brokerCxn) requestAPIVersions() error {
	maxVersion := int16(3)
start:
	req := &kmsg.ApiVersionsRequest{
		Version:               maxVersion,
		ClientSoftwareName:    cxn.cl.cfg.softwareName,
		ClientSoftwareVersion: cxn.cl.cfg.softwareVersion,
	}
	cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing api versions request", "version", maxVersion)
	corrID, err := cxn.writeRequest(nil, time.Now(), req)
	if err != nil {
		return err
	}

	rt, _ := cxn.cl.connTimeoutFn(req)
	rawResp, err := cxn.readResponse(nil, rt, time.Now(), req.Key(), corrID, false) // api versions does *not* use flexible response headers; see comment in promisedResp
	if err != nil {
		return err
	}
	if len(rawResp) < 2 {
		return ErrConnDead
	}

	resp := req.ResponseKind().(*kmsg.ApiVersionsResponse)

	// If we used a version larger than Kafka supports, Kafka replies with
	// Version 0 and an UNSUPPORTED_VERSION error.
	//
	// Pre Kafka 2.4.0, we have to retry the request with version 0.
	// Post, Kafka replies with all versions.
	if rawResp[1] == 35 {
		if maxVersion == 0 {
			return ErrConnDead
		}
		srawResp := string(rawResp)
		if srawResp == "\x00\x23\x00\x00\x00\x00" ||
			// EventHubs erroneously replies with v1, so we check
			// for that as well.
			srawResp == "\x00\x23\x00\x00\x00\x00\x00\x00\x00\x00" {
			cxn.cl.cfg.logger.Log(LogLevelDebug, "kafka does not know our ApiVersions version, downgrading to version 0 and retrying")
			maxVersion = 0
			goto start
		}
		resp.Version = 0
	}

	if err = resp.ReadFrom(rawResp); err != nil {
		return ErrConnDead
	}
	if len(resp.ApiKeys) == 0 {
		return ErrConnDead
	}

	for _, key := range resp.ApiKeys {
		if key.ApiKey > kmsg.MaxKey {
			continue
		}
		cxn.versions[key.ApiKey] = key.MaxVersion
	}
	cxn.cl.cfg.logger.Log(LogLevelDebug, "initialized api versions", "versions", cxn.versions)
	return nil
}

func (cxn *brokerCxn) sasl() error {
	if len(cxn.cl.cfg.sasls) == 0 {
		return nil
	}
	mechanism := cxn.cl.cfg.sasls[0]
	retried := false
	authenticate := false

	req := new(kmsg.SASLHandshakeRequest)
start:
	if mechanism.Name() != "GSSAPI" && cxn.versions[req.Key()] >= 0 {
		req.Mechanism = mechanism.Name()
		req.Version = cxn.versions[req.Key()]
		cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing SASLHandshakeRequest")
		corrID, err := cxn.writeRequest(nil, time.Now(), req)
		if err != nil {
			return err
		}

		rt, _ := cxn.cl.connTimeoutFn(req)
		rawResp, err := cxn.readResponse(nil, rt, time.Now(), req.Key(), corrID, req.IsFlexible())
		if err != nil {
			return err
		}
		resp := req.ResponseKind().(*kmsg.SASLHandshakeResponse)
		if err = resp.ReadFrom(rawResp); err != nil {
			return err
		}

		err = kerr.ErrorForCode(resp.ErrorCode)
		if err != nil {
			if !retried && err == kerr.UnsupportedSaslMechanism {
				for _, ours := range cxn.cl.cfg.sasls[1:] {
					for _, supported := range resp.SupportedMechanisms {
						if supported == ours.Name() {
							mechanism = ours
							retried = true
							goto start
						}
					}
				}
			}
			return err
		}
		authenticate = req.Version == 1
	}
	cxn.cl.cfg.logger.Log(LogLevelDebug, "beginning sasl authentication", "mechanism", mechanism.Name(), "authenticate", authenticate)
	cxn.mechanism = mechanism
	return cxn.doSasl(authenticate)
}

func (cxn *brokerCxn) doSasl(authenticate bool) error {
	session, clientWrite, err := cxn.mechanism.Authenticate(cxn.cl.ctx, cxn.addr)
	if err != nil {
		return err
	}
	if len(clientWrite) == 0 {
		return fmt.Errorf("unexpected server-write sasl with mechanism %s", cxn.mechanism.Name())
	}

	var lifetimeMillis int64

	// Even if we do not wrap our reads/writes in SASLAuthenticate, we
	// still use the SASLAuthenticate timeouts.
	rt, wt := cxn.cl.connTimeoutFn(new(kmsg.SASLAuthenticateRequest))

	// We continue writing until both the challenging is done AND the
	// responses are done. We can have an additional response once we
	// are done with challenges.
	step := -1
	for done := false; !done || len(clientWrite) > 0; {
		step++
		var challenge []byte

		if !authenticate {
			buf := cxn.cl.bufPool.get()

			buf = append(buf[:0], 0, 0, 0, 0)
			binary.BigEndian.PutUint32(buf, uint32(len(clientWrite)))
			buf = append(buf, clientWrite...)

			cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing raw sasl authenticate", "step", step)
			_, err, _, _ = cxn.writeConn(context.Background(), buf, wt, time.Now())

			cxn.cl.bufPool.put(buf)

			if err != nil {
				return ErrConnDead
			}
			if !done {
				if _, challenge, err, _, _ = cxn.readConn(context.Background(), rt, time.Now()); err != nil {
					return err
				}
			}

		} else {
			req := &kmsg.SASLAuthenticateRequest{
				SASLAuthBytes: clientWrite,
			}
			req.Version = cxn.versions[req.Key()]
			cxn.cl.cfg.logger.Log(LogLevelDebug, "issuing SASLAuthenticate", "version", req.Version, "step", step)

			corrID, err := cxn.writeRequest(nil, time.Now(), req)
			if err != nil {
				return err
			}
			if !done {
				rawResp, err := cxn.readResponse(nil, rt, time.Now(), req.Key(), corrID, req.IsFlexible())
				if err != nil {
					return err
				}
				resp := req.ResponseKind().(*kmsg.SASLAuthenticateResponse)
				if err = resp.ReadFrom(rawResp); err != nil {
					return err
				}

				if err = kerr.ErrorForCode(resp.ErrorCode); err != nil {
					if resp.ErrorMessage != nil {
						return fmt.Errorf("%s: %w", *resp.ErrorMessage, err)
					}
					return err
				}
				challenge = resp.SASLAuthBytes
				lifetimeMillis = resp.SessionLifetimeMillis
			}
		}

		clientWrite = nil

		if !done {
			if done, clientWrite, err = session.Challenge(challenge); err != nil {
				return err
			}
		}
	}

	if lifetimeMillis > 0 {
		// If we have a lifetime, we take 1s off of it to account
		// for some processing lag or whatever.
		// A better thing to return in the auth response would
		// have been the deadline, but we are here now.
		if lifetimeMillis < 5000 {
			return fmt.Errorf("invalid short sasl lifetime millis %d", lifetimeMillis)
		}
		cxn.expiry = time.Now().Add(time.Duration(lifetimeMillis)*time.Millisecond - time.Second)
		cxn.cl.cfg.logger.Log(LogLevelDebug, "connection has a limited lifetime", "reauthenticate_at", cxn.expiry)
	}
	return nil
}

// writeRequest writes a message request to the broker connection, bumping the
// connection's correlation ID as appropriate for the next write.
func (cxn *brokerCxn) writeRequest(ctx context.Context, enqueuedForWritingAt time.Time, req kmsg.Request) (int32, error) {
	// A nil ctx means we cannot be throttled.
	if ctx != nil {
		throttleUntil := time.Unix(0, atomic.LoadInt64(&cxn.throttleUntil))
		if sleep := throttleUntil.Sub(time.Now()); sleep > 0 {
			after := time.NewTimer(sleep)
			select {
			case <-after.C:
			case <-ctx.Done():
				after.Stop()
				return 0, ctx.Err()
			case <-cxn.cl.ctx.Done():
				after.Stop()
				return 0, ctx.Err()
			case <-cxn.deadCh:
				after.Stop()
				return 0, ErrConnDead
			}
		}
	}

	buf := cxn.cl.bufPool.get()
	defer cxn.cl.bufPool.put(buf)
	buf = cxn.cl.reqFormatter.AppendRequest(
		buf[:0],
		req,
		cxn.corrID,
	)

	_, wt := cxn.cl.connTimeoutFn(req)
	bytesWritten, writeErr, writeWait, timeToWrite := cxn.writeConn(ctx, buf, wt, enqueuedForWritingAt)

	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(BrokerWriteHook); ok {
			h.OnWrite(cxn.b.meta, req.Key(), bytesWritten, writeWait, timeToWrite, writeErr)
		}
	})

	if req.Key() == 1 && len(buf) < 1000 {
		fmt.Printf("%x\n", buf)
	}

	if writeErr != nil {
		return 0, ErrConnDead
	}
	id := cxn.corrID
	cxn.corrID++
	return id, nil
}

func (cxn *brokerCxn) writeConn(ctx context.Context, buf []byte, timeout time.Duration, enqueuedForWritingAt time.Time) (bytesWritten int, writeErr error, writeWait, timeToWrite time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		cxn.conn.SetWriteDeadline(time.Now().Add(timeout))
	}
	defer cxn.conn.SetWriteDeadline(time.Time{})
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		writeStart := time.Now()
		bytesWritten, writeErr = cxn.conn.Write(buf)
		timeToWrite = time.Since(writeStart)
		writeWait = writeStart.Sub(enqueuedForWritingAt)
	}()
	select {
	case <-writeDone:
	case <-cxn.cl.ctx.Done():
		cxn.conn.SetWriteDeadline(time.Now())
		<-writeDone
	case <-ctx.Done():
		cxn.conn.SetWriteDeadline(time.Now())
		<-writeDone
	}
	return
}

func (cxn *brokerCxn) readConn(ctx context.Context, timeout time.Duration, enqueuedForReadingAt time.Time) (nread int, buf []byte, err error, readWait, timeToRead time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout > 0 {
		fmt.Println(time.Now(), "set read deadline", timeout)
		cxn.conn.SetReadDeadline(time.Now().Add(timeout))
	}
	defer fmt.Println(time.Now(), "clearing read deadline")
	defer cxn.conn.SetReadDeadline(time.Time{})
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sizeBuf := make([]byte, 4)
		readStart := time.Now()
		defer func() {
			timeToRead = time.Since(readStart)
			readWait = readStart.Sub(enqueuedForReadingAt)
		}()
		if nread, err = io.ReadFull(cxn.conn, sizeBuf); err != nil {
			err = ErrConnDead
			return
		}
		size := int32(binary.BigEndian.Uint32(sizeBuf))
		if size < 0 {
			err = ErrInvalidRespSize
			return
		}
		if maxSize := cxn.b.cl.cfg.maxBrokerReadBytes; size > maxSize {
			err = &ErrLargeRespSize{Size: size, Limit: maxSize}
			return
		}
		buf = make([]byte, size)
		var nread2 int
		fmt.Println(time.Now(), "reading full responsE")
		nread2, err = io.ReadFull(cxn.conn, buf)
		nread += nread2
		buf = buf[:nread2]
		if err != nil {
			fmt.Println(time.Now(), "read full err", err)
			err = ErrConnDead
			return
		}
	}()
	select {
	case <-readDone:
	case <-cxn.cl.ctx.Done():
		fmt.Println("client ctx done killing read")
		cxn.conn.SetReadDeadline(time.Now())
		<-readDone
	case <-ctx.Done():
		fmt.Println("req ctx done killing read")
		cxn.conn.SetReadDeadline(time.Now())
		<-readDone
	}
	return
}

// readResponse reads a response from conn, ensures the correlation ID is
// correct, and returns a newly allocated slice on success.
func (cxn *brokerCxn) readResponse(ctx context.Context, timeout time.Duration, enqueuedForReadingAt time.Time, key int16, corrID int32, flexibleHeader bool) ([]byte, error) {
	nread, buf, err, readWait, timeToRead := cxn.readConn(ctx, timeout, enqueuedForReadingAt)

	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(BrokerReadHook); ok {
			h.OnRead(cxn.b.meta, key, nread, readWait, timeToRead, err)
		}
	})

	if key == 1 && len(buf) < 1000 {
		fmt.Printf("%x\n", buf)
	}

	if err != nil {
		return nil, err
	}
	if len(buf) < 4 {
		return nil, kbin.ErrNotEnoughData
	}
	gotID := int32(binary.BigEndian.Uint32(buf))
	if gotID != corrID {
		return nil, ErrCorrelationIDMismatch
	}
	// If the response header is flexible, we skip the tags at the end of
	// it. They are currently unused.
	if flexibleHeader {
		b := kbin.Reader{Src: buf[4:]}
		kmsg.SkipTags(&b)
		return b.Src, b.Complete()
	}
	return buf[4:], nil
}

// closeConn is the one place we close broker connections. This is always done
// in either die, which is called when handleResps returns, or if init fails,
// which means we did not succeed enough to start handleResps.
func (cxn *brokerCxn) closeConn() {
	cxn.cl.cfg.hooks.each(func(h Hook) {
		if h, ok := h.(BrokerDisconnectHook); ok {
			h.OnDisconnect(cxn.b.meta, cxn.conn)
		}
	})
	cxn.conn.Close()
	close(cxn.deadCh)
}

// die kills a broker connection (which could be dead already) and replies to
// all requests awaiting responses appropriately.
func (cxn *brokerCxn) die() {
	if cxn == nil {
		return
	}
	if atomic.SwapInt32(&cxn.dead, 1) == 1 {
		return
	}

	cxn.closeConn()

	go func() {
		for pr := range cxn.resps {
			pr.promise(nil, ErrConnDead)
		}
	}()

	cxn.dieMu.Lock()
	cxn.dieMu.Unlock()

	close(cxn.resps) // after lock, nothing sends down resps
}

// waitResp, called serially by a broker's handleReqs, manages handling a
// message requests's response.
func (cxn *brokerCxn) waitResp(pr promisedResp) {
	dead := false

	cxn.dieMu.RLock()
	if atomic.LoadInt32(&cxn.dead) == 1 {
		dead = true
	} else {
		cxn.resps <- pr
	}
	cxn.dieMu.RUnlock()

	if dead {
		pr.promise(nil, ErrConnDead)
	}
}

// handleResps serially handles all broker responses for an single connection.
func (cxn *brokerCxn) handleResps() {
	defer cxn.die() // always track our death

	var successes uint64
	for pr := range cxn.resps {
		raw, err := cxn.readResponse(pr.ctx, pr.readTimeout, pr.enqueue, pr.resp.Key(), pr.corrID, pr.flexibleHeader)
		if err != nil {
			if successes > 0 || len(cxn.b.cl.cfg.sasls) > 0 {
				cxn.b.cl.cfg.logger.Log(LogLevelDebug, "read from broker errored, killing connection", "addr", cxn.b.addr, "id", cxn.b.meta.NodeID, "successful_reads", successes, "err", err)
			} else {
				cxn.b.cl.cfg.logger.Log(LogLevelWarn, "read from broker errored, killing connection after 0 successful responses (is sasl missing?)", "addr", cxn.b.addr, "id", cxn.b.meta.NodeID, "err", err)
			}
			pr.promise(nil, err)
			return
		}
		successes++
		readErr := pr.resp.ReadFrom(raw)

		// If we had no error, we read the response successfully.
		//
		// Any response that can cause throttling satisfies the
		// kmsg.ThrottleResponse interface. We check that here.
		if readErr == nil {
			if throttleResponse, ok := pr.resp.(kmsg.ThrottleResponse); ok {
				millis, throttlesAfterResp := throttleResponse.Throttle()
				if millis > 0 {
					if throttlesAfterResp {
						throttleUntil := time.Now().Add(time.Millisecond * time.Duration(millis)).UnixNano()
						if throttleUntil > cxn.throttleUntil {
							atomic.StoreInt64(&cxn.throttleUntil, throttleUntil)
						}
					}
					cxn.cl.cfg.hooks.each(func(h Hook) {
						if h, ok := h.(BrokerThrottleHook); ok {
							h.OnThrottle(cxn.b.meta, time.Duration(millis)*time.Millisecond, throttlesAfterResp)
						}
					})
				}
			}
		}

		pr.promise(pr.resp, readErr)
	}
}
