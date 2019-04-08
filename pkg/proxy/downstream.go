/*
 * Licensed to the Apache Software Foundation (ASF) under one or more
 * contributor license agreements.  See the NOTICE file distributed with
 * this work for additional information regarding copyright ownership.
 * The ASF licenses this file to You under the Apache License, Version 2.0
 * (the "License"); you may not use this file except in compliance with
 * the License.  You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package proxy

import (
	"container/list"
	"context"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/alipay/sofa-mosn/pkg/api/v2"
	"github.com/alipay/sofa-mosn/pkg/trace"
	"github.com/alipay/sofa-mosn/pkg/utils"

	"runtime/debug"

	"github.com/alipay/sofa-mosn/pkg/buffer"
	"github.com/alipay/sofa-mosn/pkg/log"
	"github.com/alipay/sofa-mosn/pkg/protocol"
	"github.com/alipay/sofa-mosn/pkg/protocol/http"
	"github.com/alipay/sofa-mosn/pkg/router"
	"github.com/alipay/sofa-mosn/pkg/types"
)

// types.StreamEventListener
// types.StreamReceiveListener
// types.FilterChainFactoryCallbacks
// Downstream stream, as a controller to handle downstream and upstream proxy flow
type downStream struct {
	ID      uint32
	proxy   *proxy
	route   types.Route
	cluster types.ClusterInfo
	element *list.Element

	// flow control
	bufferLimit uint32

	// ~~~ control args
	timeout    *Timeout
	retryState *retryState

	requestInfo     types.RequestInfo
	responseSender  types.StreamSender
	upstreamRequest *upstreamRequest
	perRetryTimer   *utils.Timer
	responseTimer   *utils.Timer

	// ~~~ downstream request buf
	downstreamReqHeaders  types.HeaderMap
	downstreamReqDataBuf  types.IoBuffer
	downstreamReqTrailers types.HeaderMap

	// ~~~ downstream response buf
	downstreamRespHeaders  types.HeaderMap
	downstreamRespDataBuf  types.IoBuffer
	downstreamRespTrailers types.HeaderMap

	// ~~~ state
	// starts to send back downstream response, set on upstream response detected
	downstreamResponseStarted bool
	// downstream request received done
	downstreamRecvDone bool
	// upstream req sent
	upstreamRequestSent bool
	// 1. at the end of upstream response 2. by a upstream reset due to exceptions, such as no healthy upstream, connection close, etc.
	upstreamProcessDone bool
	// don't convert headers, data and trailers.  e.g. activeStreamReceiverFilter.Appendxx
	noConvert bool
	// direct response.  e.g. sendHijack
	directResponse bool

	notify chan struct{}

	downstreamReset   uint32
	downstreamCleaned uint32
	upstreamReset     uint32
	reuseBuffer       uint32

	resetReason types.StreamResetReason

	//filters
	senderFilters        []*activeStreamSenderFilter
	senderFiltersIndex   int
	receiverFilters      []*activeStreamReceiverFilter
	receiverFiltersIndex int
	receiverFiltersAgain bool

	context context.Context

	// stream access logs
	streamAccessLogs []types.AccessLog
	logger           log.ErrorLogger
	logDone          uint32

	snapshot types.ClusterSnapshot
}

func newActiveStream(ctx context.Context, proxy *proxy, responseSender types.StreamSender, spanBuilder types.SpanBuilder) *downStream {
	if spanBuilder != nil && trace.IsTracingEnabled() {
		span := spanBuilder.BuildSpan(ctx)
		if span != nil {
			ctx = context.WithValue(ctx, trace.ActiveSpanKey, span)
			ctx = context.WithValue(ctx, types.ContextKeyTraceSpanKey, &trace.SpanKey{TraceId: span.TraceId(), SpanId: span.SpanId()})
		}
	}

	proxyBuffers := proxyBuffersByContext(ctx)

	stream := &proxyBuffers.stream
	stream.ID = atomic.AddUint32(&currProxyID, 1)
	stream.proxy = proxy
	stream.requestInfo = &proxyBuffers.info
	stream.requestInfo.SetStartTime()
	stream.responseSender = responseSender
	stream.responseSender.GetStream().AddEventListener(stream)
	stream.context = ctx
	stream.reuseBuffer = 1
	stream.notify = make(chan struct{}, 1)

	stream.logger = log.ByContext(proxy.context)

	proxy.stats.DownstreamRequestTotal.Inc(1)
	proxy.stats.DownstreamRequestActive.Inc(1)
	proxy.listenerStats.DownstreamRequestTotal.Inc(1)
	proxy.listenerStats.DownstreamRequestActive.Inc(1)

	// debug message for downstream
	stream.logger.Debugf("client conn id %d, proxy id %d, downstream id %d", proxy.readCallbacks.Connection().ID(), stream.ID, responseSender.GetStream().ID())
	return stream
}

// downstream's lifecycle ends normally
func (s *downStream) endStream() {
	if s.responseSender != nil && !s.downstreamRecvDone {
		// not reuse buffer
		atomic.StoreUint32(&s.reuseBuffer, 0)
	}
	s.cleanStream()

	// note: if proxy logic resets the stream, there maybe some underlying data in the conn.
	// we ignore this for now, fix as a todo
}

// Clean up on the very end of the stream: end stream or reset stream
// Resources to clean up / reset:
// 	+ upstream request
// 	+ all timers
// 	+ all filters
//  + remove stream in proxy context
func (s *downStream) cleanStream() {
	if !atomic.CompareAndSwapUint32(&s.downstreamCleaned, 0, 1) {
		return
	}

	s.requestInfo.SetRequestFinishedDuration(time.Now())

	streamDurationNs := s.requestInfo.RequestFinishedDuration().Nanoseconds()

	// reset corresponding upstream stream
	if s.upstreamRequest != nil && !s.upstreamProcessDone {
		s.logger.Errorf("downStream upstreamRequest resetStream id: %d", s.ID)
		s.upstreamProcessDone = true
		s.upstreamRequest.resetStream()
	}

	// clean up timers
	s.cleanUp()

	// tell filters it's time to destroy
	for _, ef := range s.senderFilters {
		ef.filter.OnDestroy()
	}

	for _, ef := range s.receiverFilters {
		ef.filter.OnDestroy()
	}

	// countdown metrics
	s.proxy.stats.DownstreamRequestActive.Dec(1)
	s.proxy.stats.DownstreamRequestTime.Update(streamDurationNs)
	s.proxy.stats.DownstreamRequestTimeTotal.Inc(streamDurationNs)

	s.proxy.listenerStats.DownstreamRequestActive.Dec(1)
	s.proxy.listenerStats.DownstreamRequestTime.Update(streamDurationNs)
	s.proxy.listenerStats.DownstreamRequestTimeTotal.Inc(streamDurationNs)

	// finish tracing
	s.finishTracing()

	// write access log
	s.writeLog()

	// delete stream
	s.proxy.deleteActiveStream(s)

	// recycle if no reset events
	s.giveStream()
}

func (s *downStream) writeLog() {
	defer func() {
		if r := recover(); r != nil {
			log.DefaultLogger.Errorf("downStream writeLog panic %v, downstream %+v", r, s)
		}
	}()

	if !atomic.CompareAndSwapUint32(&s.logDone, 0, 1) {
		return
	}
	// proxy access log
	if s.proxy != nil && s.proxy.accessLogs != nil {
		for _, al := range s.proxy.accessLogs {
			al.Log(s.downstreamReqHeaders, s.downstreamRespHeaders, s.requestInfo)
		}
	}

	// per-stream access log
	if s.streamAccessLogs != nil {
		for _, al := range s.streamAccessLogs {
			al.Log(s.downstreamReqHeaders, s.downstreamRespHeaders, s.requestInfo)
		}
	}
}

// types.StreamEventListener
// Called by stream layer normally
func (s *downStream) OnResetStream(reason types.StreamResetReason) {
	if !atomic.CompareAndSwapUint32(&s.downstreamReset, 0, 1) {
		return
	}

	s.resetReason = reason

	s.sendNotify()
}

func (s *downStream) ResetStream(reason types.StreamResetReason) {
	s.proxy.stats.DownstreamRequestReset.Inc(1)
	s.proxy.listenerStats.DownstreamRequestReset.Inc(1)
	s.cleanStream()
}

func (s *downStream) OnDestroyStream() {}

// types.StreamReceiveListener
func (s *downStream) OnReceive(ctx context.Context, headers types.HeaderMap, data types.IoBuffer, trailers types.HeaderMap) {
	s.downstreamReqHeaders = headers
	if data != nil {
		s.downstreamReqDataBuf = data.Clone()
		data.Drain(data.Len())
	}
	s.downstreamReqTrailers = trailers

	id := s.ID
	// goroutine for proxy
	pool.ScheduleAuto(func() {
		defer func() {
			if r := recover(); r != nil {
				log.DefaultLogger.Errorf("downStream OnReceive panic %v, downstream %+v old id: %d, new id: %d",
					r, s, id, s.ID)
				debug.PrintStack()

				if id == s.ID {
					s.writeLog()
				}
			}
		}()

		phase := types.InitPhase
		for i := 0; i < 5; i++ {
			s.cleanNotify()

			phase = s.receive(ctx, id, phase)
			switch phase {
			case types.End:
				return
			case types.MatchRoute:
				s.logger.Debugf("downstream redo match route %+v", s)
			case types.Retry:
				s.logger.Debugf("downstream retry %+v", s)
			case types.UpFilter:
				s.logger.Debugf("downstream directResponse %+v", s)
			}
		}
	})
}

func (s *downStream) receive(ctx context.Context, id uint32, phase types.Phase) types.Phase {
	s.logger.Tracef("downstream OnReceive send upstream request %+v", s)

	switch phase {
	// init phase
	case types.InitPhase:
		phase++
		fallthrough

	// downstream filter before route
	case types.DownFilter:
		s.logger.Tracef("downStream Phase %d, id %d", phase, id)
		s.runReceiveFilters(phase, s.downstreamReqHeaders, s.downstreamReqDataBuf, s.downstreamReqTrailers)

		if p, err := s.processError(id); err != nil {
			return p
		}
		phase++
		fallthrough

	// match route
	case types.MatchRoute:
		s.logger.Tracef("downStream Phase %d, id %d", phase, id)
		s.matchRoute()
		if p, err := s.processError(id); err != nil {
			return p
		}
		phase++
		fallthrough

	// downstream filter after route
	case types.DownFilterAfterRoute:
		s.logger.Tracef("downStream Phase %d, id %d", phase, id)
		s.runReceiveFilters(phase, s.downstreamReqHeaders, s.downstreamReqDataBuf, s.downstreamReqTrailers)

		if p, err := s.processError(id); err != nil {
			return p
		}
		phase++
		fallthrough

	// downstream receive header
	case types.DownRecvHeader:
		if s.downstreamReqHeaders != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.receiveHeaders(s.downstreamReqDataBuf == nil && s.downstreamReqTrailers == nil)

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		phase++
		fallthrough

	// downstream receive data
	case types.DownRecvData:
		if s.downstreamReqDataBuf != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.downstreamReqDataBuf.Count(1)
			s.receiveData(s.downstreamReqTrailers == nil)

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		phase++
		fallthrough

	// downstream receive trailer
	case types.DownRecvTrailer:
		if s.downstreamReqTrailers != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.receiveTrailers()

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		// skip types.Retry
		phase = types.WaitNofity
		fallthrough

	// retry request
	case types.Retry:
		if phase == types.Retry {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)

			if s.downstreamReqDataBuf != nil {
				s.downstreamReqDataBuf.Count(1)
			}
			s.doRetry()
			if p, err := s.processError(id); err != nil {
				return p
			}
			phase++
		}
		fallthrough

	// wait for upstreamRequest or reset
	case types.WaitNofity:
		s.logger.Tracef("downStream Phase %d, id %d", phase, id)
		if p, err := s.waitNotify(id); err != nil {
			return p
		}

		s.logger.Tracef("downstream OnReceive send downstream response %+v", s.downstreamRespHeaders)

		phase++
		fallthrough

	// upstream filter
	case types.UpFilter:
		s.logger.Tracef("downStream Phase %d, id %d", phase, id)
		s.runAppendFilters(phase, s.downstreamRespHeaders, s.downstreamRespDataBuf, s.downstreamRespTrailers)

		if p, err := s.processError(id); err != nil {
			return p
		}

		// directResponse
		if s.upstreamRequest == nil {
			fakeUpstreamRequest := &upstreamRequest{
				downStream: s,
			}

			s.upstreamRequest = fakeUpstreamRequest
		}

		phase++
		fallthrough

	// upstream receive header
	case types.UpRecvHeader:
		// send downstream response
		if s.downstreamRespHeaders != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.upstreamRequest.receiveHeaders(s.downstreamRespDataBuf == nil && s.downstreamRespTrailers == nil)

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		phase++
		fallthrough

	// upstream receive data
	case types.UpRecvData:
		if s.downstreamRespDataBuf != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.upstreamRequest.receiveData(s.downstreamRespTrailers == nil)

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		phase++
		fallthrough

	// upstream receive triler
	case types.UpRecvTrailer:
		if s.downstreamRespTrailers != nil {
			s.logger.Tracef("downStream Phase %d, id %d", phase, id)
			s.upstreamRequest.receiveTrailers()

			if p, err := s.processError(id); err != nil {
				return p
			}
		}
		phase++
		fallthrough

	// process end
	case types.End:
		return types.End

	default:
		s.logger.Errorf("unexpected phase: %d", phase)
		return types.End
	}
}

func (s *downStream) matchRoute() {
	log.DefaultLogger.Tracef("before active stream route")
	headers := s.downstreamReqHeaders
	if s.proxy.routersWrapper == nil || s.proxy.routersWrapper.GetRouters() == nil {
		log.DefaultLogger.Errorf("doReceiveHeaders error: routersWrapper or routers in routersWrapper is nil")
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, headers)
		return
	}

	// get router instance and do routing
	routers := s.proxy.routersWrapper.GetRouters()
	// do handler chain
	handlerChain := router.CallMakeHandlerChain(s.context, headers, routers, s.proxy.clusterManager)
	// handlerChain should never be nil
	if handlerChain == nil {
		log.DefaultLogger.Errorf("no route to make handler chain, headers = %v", headers)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, headers)
		return
	}
	s.snapshot, s.route = handlerChain.DoNextHandler()
}

func (s *downStream) convertProtocol() (dp, up types.Protocol) {
	dp = s.getDownstreamProtocol()
	up = s.getUpstreamProtocol()
	return
}

func (s *downStream) getDownstreamProtocol() (prot types.Protocol) {
	if s.proxy.serverStreamConn == nil {
		prot = types.Protocol(s.proxy.config.DownstreamProtocol)
	} else {
		prot = s.proxy.serverStreamConn.Protocol()
	}
	return prot
}

func (s *downStream) getUpstreamProtocol() (currentProtocol types.Protocol) {
	configProtocol := s.proxy.config.UpstreamProtocol

	// if route exists upstream protocol, it will replace the proxy config's upstream protocol
	if s.route != nil && s.route.RouteRule() != nil && s.route.RouteRule().UpstreamProtocol() != "" {
		configProtocol = s.route.RouteRule().UpstreamProtocol()
	}

	// Auto means same as downstream protocol
	if configProtocol == string(protocol.Auto) {
		currentProtocol = s.getDownstreamProtocol()
	} else {
		currentProtocol = types.Protocol(configProtocol)
	}

	return currentProtocol
}

func (s *downStream) receiveHeaders(endStream bool) {
	s.downstreamRecvDone = endStream

	// after stream filters run, check the route
	if s.route == nil {
		log.DefaultLogger.Warnf("no route to init upstream,headers = %v", s.downstreamReqHeaders)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)

		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)

		return
	}
	// check if route have direct response
	// direct response will response now
	if resp := s.route.DirectResponseRule(); !(resp == nil || reflect.ValueOf(resp).IsNil()) {
		log.DefaultLogger.Infof("direct response for stream , id = %d", s.ID)
		if resp.Body() != "" {
			s.sendHijackReplyWithBody(resp.StatusCode(), s.downstreamReqHeaders, resp.Body())
		} else {
			s.sendHijackReply(resp.StatusCode(), s.downstreamReqHeaders)
		}
		return
	}
	// not direct response, needs a cluster snapshot and route rule
	if rule := s.route.RouteRule(); rule == nil || reflect.ValueOf(rule).IsNil() {
		log.DefaultLogger.Warnf("no route rule to init upstream, headers = %v", s.downstreamReqHeaders)
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)
		return
	}
	if s.snapshot == nil || reflect.ValueOf(s.snapshot).IsNil() {
		// no available cluster
		log.DefaultLogger.Errorf("cluster snapshot is nil, cluster name is: %s", s.route.RouteRule().ClusterName())
		s.requestInfo.SetResponseFlag(types.NoRouteFound)
		s.sendHijackReply(types.RouterUnavailableCode, s.downstreamReqHeaders)
		return
	}
	// as ClusterName has random factor when choosing weighted cluster,
	// so need determination at the first time
	clusterName := s.route.RouteRule().ClusterName()
	log.DefaultLogger.Tracef("get route : %v,clusterName=%v", s.route, clusterName)

	s.cluster = s.snapshot.ClusterInfo()

	s.requestInfo.SetRouteEntry(s.route.RouteRule())
	s.requestInfo.SetDownstreamLocalAddress(s.proxy.readCallbacks.Connection().LocalAddr())
	// todo: detect remote addr
	s.requestInfo.SetDownstreamRemoteAddress(s.proxy.readCallbacks.Connection().RemoteAddr())

	// `downstream` implement loadbalancer ctx
	log.DefaultLogger.Tracef("before initializeUpstreamConnectionPool")
	pool, err := s.initializeUpstreamConnectionPool(s)

	if err != nil {
		log.DefaultLogger.Errorf("initialize Upstream Connection Pool error, request can't be proxyed,error = %v", err)
		return
	}

	log.DefaultLogger.Tracef("after initializeUpstreamConnectionPool")
	s.timeout = parseProxyTimeout(s.route, s.downstreamReqHeaders)

	prot := s.getUpstreamProtocol()

	s.retryState = newRetryState(s.route.RouteRule().Policy().RetryPolicy(), s.downstreamReqHeaders, s.cluster, prot)

	//Build Request
	proxyBuffers := proxyBuffersByContext(s.context)
	s.upstreamRequest = &proxyBuffers.request
	s.upstreamRequest.downStream = s
	s.upstreamRequest.proxy = s.proxy
	s.upstreamRequest.protocol = prot
	s.upstreamRequest.connPool = pool
	s.route.RouteRule().FinalizeRequestHeaders(s.downstreamReqHeaders, s.requestInfo)

	//Call upstream's append header method to build upstream's request
	s.upstreamRequest.appendHeaders(endStream)

	if endStream {
		s.onUpstreamRequestSent()
	}
}

func (s *downStream) receiveData(endStream bool) {
	// if active stream finished before receive data, just ignore further data
	if s.processDone() {
		return
	}
	data := s.downstreamReqDataBuf
	log.DefaultLogger.Tracef("downstream receive data = %v", data)

	s.requestInfo.SetBytesReceived(s.requestInfo.BytesReceived() + uint64(data.Len()))
	s.downstreamRecvDone = endStream

	if endStream {
		s.onUpstreamRequestSent()
	}

	s.upstreamRequest.appendData(endStream)

	// if upstream process done in the middle of receiving data, just end stream
	if s.upstreamProcessDone {
		s.cleanStream()
	}
}

func (s *downStream) receiveTrailers() {
	// if active stream finished the lifecycle, just ignore further data
	if s.processDone() {
		return
	}

	s.downstreamRecvDone = true

	s.onUpstreamRequestSent()
	s.upstreamRequest.appendTrailers()

	// if upstream process done in the middle of receiving trailers, just end stream
	if s.upstreamProcessDone {
		s.cleanStream()
	}
}

func (s *downStream) OnDecodeError(context context.Context, err error, headers types.HeaderMap) {
	// if active stream finished the lifecycle, just ignore further data
	if s.upstreamProcessDone {
		return
	}

	// todo: enrich headers' information to do some hijack
	// Check headers' info to do hijack
	switch err.Error() {
	case types.CodecException:
		s.sendHijackReply(types.CodecExceptionCode, headers)
	case types.DeserializeException:
		s.sendHijackReply(types.DeserialExceptionCode, headers)
	default:
		s.sendHijackReply(types.UnknownCode, headers)
	}
}

func (s *downStream) onUpstreamRequestSent() {
	s.upstreamRequestSent = true
	s.requestInfo.SetRequestReceivedDuration(time.Now())

	if s.upstreamRequest != nil {
		// setup per req timeout timer
		s.setupPerReqTimeout()

		// setup global timeout timer
		if s.timeout.GlobalTimeout > 0 {
			log.DefaultLogger.Debugf("start a request timeout timer")
			if s.responseTimer != nil {
				s.responseTimer.Stop()
			}

			ID := s.ID
			s.responseTimer = utils.NewTimer(s.timeout.GlobalTimeout,
				func() {
					if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
						return
					}
					if ID != s.ID {
						return
					}
					s.onResponseTimeout()
				})
		}
	}
}

// Note: global-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) onResponseTimeout() {
	defer func() {
		if r := recover(); r != nil {
			log.DefaultLogger.Errorf("onResponseTimeout() panic %v", r)
		}
	}()
	s.responseTimer = nil
	s.cluster.Stats().UpstreamRequestTimeout.Inc(1)

	if s.upstreamRequest != nil {
		if s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamRequestTimeout.Inc(1)
		}

		atomic.StoreUint32(&s.reuseBuffer, 0)
		s.upstreamRequest.resetStream()
		s.upstreamRequest.OnResetStream(types.UpstreamGlobalTimeout)
	}
}

func (s *downStream) setupPerReqTimeout() {
	timeout := s.timeout

	if timeout.TryTimeout > 0 {
		if s.perRetryTimer != nil {
			s.perRetryTimer.Stop()
		}

		ID := s.ID
		s.perRetryTimer = utils.NewTimer(timeout.TryTimeout,
			func() {
				if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
					return
				}
				if ID != s.ID {
					return
				}
				s.onPerReqTimeout()
			})
	}
}

// Note: per-try-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) onPerReqTimeout() {
	defer func() {
		if r := recover(); r != nil {
			log.DefaultLogger.Errorf("onPerReqTimeout() panic %v", r)
		}
	}()

	if !s.downstreamResponseStarted {
		// handle timeout on response not

		s.perRetryTimer = nil
		s.cluster.Stats().UpstreamRequestTimeout.Inc(1)

		if s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamRequestTimeout.Inc(1)
		}

		atomic.StoreUint32(&s.reuseBuffer, 0)
		s.upstreamRequest.resetStream()
		s.requestInfo.SetResponseFlag(types.UpstreamRequestTimeout)
		s.upstreamRequest.OnResetStream(types.UpstreamPerTryTimeout)
	} else {
		log.DefaultLogger.Debugf("Skip request timeout on getting upstream response")
	}
}

func (s *downStream) initializeUpstreamConnectionPool(lbCtx types.LoadBalancerContext) (types.ConnectionPool, error) {
	var connPool types.ConnectionPool

	currentProtocol := s.getUpstreamProtocol()

	connPool = s.proxy.clusterManager.ConnPoolForCluster(lbCtx, s.snapshot, currentProtocol)

	if connPool == nil {
		s.requestInfo.SetResponseFlag(types.NoHealthyUpstream)
		s.sendHijackReply(types.NoHealthUpstreamCode, s.downstreamReqHeaders)

		return nil, fmt.Errorf("no healthy upstream in cluster %s", s.cluster.Name())
	}

	// TODO: update upstream stats

	return connPool, nil
}

// ~~~ active stream sender wrapper

func (s *downStream) appendHeaders(endStream bool) {
	s.upstreamProcessDone = endStream
	headers := s.convertHeader(s.downstreamRespHeaders)
	//Currently, just log the error
	if err := s.responseSender.AppendHeaders(s.context, headers, endStream); err != nil {
		s.logger.Errorf("[downstream] append headers error, %s", err)
	}

	if endStream {
		s.endStream()
	}
}

func (s *downStream) convertHeader(headers types.HeaderMap) types.HeaderMap {
	if s.noConvert {
		return headers
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convHeader, err := protocol.ConvertHeader(s.context, up, dp, headers); err == nil {
			return convHeader
		} else {
			s.logger.Warnf("convert header from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return headers
}

func (s *downStream) appendData(endStream bool) {
	s.upstreamProcessDone = endStream

	data := s.convertData(s.downstreamRespDataBuf)
	s.requestInfo.SetBytesSent(s.requestInfo.BytesSent() + uint64(data.Len()))
	s.responseSender.AppendData(s.context, data, endStream)

	if endStream {
		s.endStream()
	}
}

func (s *downStream) convertData(data types.IoBuffer) types.IoBuffer {
	if s.noConvert {
		return data
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convData, err := protocol.ConvertData(s.context, up, dp, data); err == nil {
			return convData
		} else {
			s.logger.Warnf("convert data from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return data
}

func (s *downStream) appendTrailers() {
	s.upstreamProcessDone = true
	trailers := s.convertTrailer(s.downstreamRespTrailers)
	s.responseSender.AppendTrailers(s.context, trailers)
	s.endStream()
}

func (s *downStream) convertTrailer(trailers types.HeaderMap) types.HeaderMap {
	if s.noConvert {
		return trailers
	}

	dp, up := s.convertProtocol()

	// need protocol convert
	if dp != up {
		if convTrailer, err := protocol.ConvertTrailer(s.context, up, dp, trailers); err == nil {
			return convTrailer
		} else {
			s.logger.Warnf("convert header from %s to %s failed, %s", up, dp, err.Error())
		}
	}
	return trailers
}

// ~~~ upstream event handler
func (s *downStream) onUpstreamReset(reason types.StreamResetReason) {
	// todo: update stats
	s.logger.Errorf("on upstream reset invoked reason %v", reason)

	// see if we need a retry
	if reason != types.UpstreamGlobalTimeout &&
		!s.downstreamResponseStarted && s.retryState != nil {
		retryCheck := s.retryState.retry(nil, reason)

		if retryCheck == types.ShouldRetry && s.setupRetry(true) {
			if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
				s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
				s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
			}

			// setup retry timer and return
			// clear reset flag
			s.logger.Errorf("on upstream doRetry reason %v", reason)
			atomic.CompareAndSwapUint32(&s.upstreamReset, 1, 0)
			return
		} else if retryCheck == types.RetryOverflow {
			s.requestInfo.SetResponseFlag(types.UpstreamOverflow)
		}
	}

	// clean up all timers
	s.cleanUp()

	// If we have not yet sent anything downstream, send a response with an appropriate status code.
	// Otherwise just reset the ongoing response.
	if s.downstreamResponseStarted {
		s.resetStream()
	} else {
		// send err response if response not started
		var code int

		if reason == types.UpstreamGlobalTimeout || reason == types.UpstreamPerTryTimeout {
			s.requestInfo.SetResponseFlag(types.UpstreamRequestTimeout)
			code = types.TimeoutExceptionCode
		} else {
			reasonFlag := s.proxy.streamResetReasonToResponseFlag(reason)
			s.requestInfo.SetResponseFlag(reasonFlag)
			code = types.NoHealthUpstreamCode
		}

		if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
			s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
		}
		// clear reset flag
		s.logger.Errorf("on upstream hijack reason %v", reason)
		atomic.CompareAndSwapUint32(&s.upstreamReset, 1, 0)
		s.sendHijackReply(code, s.downstreamReqHeaders)
	}
}

func (s *downStream) onUpstreamHeaders(endStream bool) {
	headers := s.downstreamRespHeaders

	// check retry
	if s.retryState != nil {
		retryCheck := s.retryState.retry(headers, "")

		if retryCheck == types.ShouldRetry && s.setupRetry(endStream) {
			if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
				s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
				s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
			}

			return
		} else if retryCheck == types.RetryOverflow {
			s.requestInfo.SetResponseFlag(types.UpstreamOverflow)
		}

		s.retryState.reset()
	}

	s.handleUpstreamStatusCode()

	s.downstreamResponseStarted = true

	// directResponse for no route should be nil
	if s.route != nil {
		s.route.RouteRule().FinalizeResponseHeaders(headers, s.requestInfo)
	}

	if endStream {
		s.onUpstreamResponseRecvFinished()
	}

	// todo: insert proxy headers
	s.appendHeaders(endStream)
}

func (s *downStream) handleUpstreamStatusCode() {
	// todo: support config?
	if s.upstreamRequest != nil && s.upstreamRequest.host != nil {
		if s.requestInfo.ResponseCode() >= http.InternalServerError {
			s.upstreamRequest.host.HostStats().UpstreamResponseFailed.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseFailed.Inc(1)
		} else {
			s.upstreamRequest.host.HostStats().UpstreamResponseSuccess.Inc(1)
			s.upstreamRequest.host.ClusterInfo().Stats().UpstreamResponseSuccess.Inc(1)
		}
	}
}

func (s *downStream) onUpstreamData(endStream bool) {
	if endStream {
		s.onUpstreamResponseRecvFinished()
	}

	s.appendData(endStream)
}

func (s *downStream) finishTracing() {
	if trace.IsTracingEnabled() {
		if s.context == nil {
			return
		}
		span := trace.SpanFromContext(s.context)

		if span != nil {
			span.SetTag(trace.REQUEST_SIZE, strconv.FormatInt(int64(s.requestInfo.BytesSent()), 10))
			span.SetTag(trace.RESPONSE_SIZE, strconv.FormatInt(int64(s.requestInfo.BytesReceived()), 10))
			if s.requestInfo.UpstreamHost() != nil {
				span.SetTag(trace.UPSTREAM_HOST_ADDRESS, s.requestInfo.UpstreamHost().AddressString())
			}
			if s.requestInfo.DownstreamLocalAddress() != nil {
				span.SetTag(trace.DOWNSTEAM_HOST_ADDRESS, s.requestInfo.DownstreamRemoteAddress().String())
			}
			span.SetTag(trace.RESULT_STATUS, fmt.Sprint(s.requestInfo.ResponseCode()))
			span.FinishSpan()

			if s.context.Value(types.ContextKeyListenerType) == v2.INGRESS {
				trace.DeleteSpanIdGenerator(s.context.Value(types.ContextKeyTraceSpanKey).(*trace.SpanKey))
			}
		} else {
			log.DefaultLogger.Debugf("Span is null")
		}
	}
}

func (s *downStream) onUpstreamTrailers() {
	s.onUpstreamResponseRecvFinished()

	s.appendTrailers()
}

func (s *downStream) onUpstreamResponseRecvFinished() {
	if !s.upstreamRequestSent {
		s.upstreamRequest.resetStream()
	}

	// todo: stats
	// todo: logs

	s.cleanUp()
}

func (s *downStream) setupRetry(endStream bool) bool {
	s.upstreamRequest.setupRetry = true

	if !endStream {
		s.upstreamRequest.resetStream()
	}

	// reset per req timer
	if s.perRetryTimer != nil {
		s.perRetryTimer.Stop()
		s.perRetryTimer = nil
	}

	return true
}

// Note: retry-timer MUST be stopped before active stream got recycled, otherwise resetting stream's properties will cause panic here
func (s *downStream) doRetry() {
	// no reuse buffer
	atomic.StoreUint32(&s.reuseBuffer, 0)

	pool, err := s.initializeUpstreamConnectionPool(s)

	if err != nil {
		s.sendHijackReply(types.NoHealthUpstreamCode, s.downstreamReqHeaders)
		s.cleanUp()
		return
	}

	s.upstreamRequest = &upstreamRequest{
		downStream: s,
		proxy:      s.proxy,
		connPool:   pool,
	}

	// if Data or Trailer exists, endStream should be false, else should be true
	s.upstreamRequest.appendHeaders(s.downstreamReqDataBuf == nil && s.downstreamReqTrailers == nil)

	if s.downstreamReqDataBuf != nil {
		s.upstreamRequest.appendData(s.downstreamReqTrailers == nil)
	}

	if s.downstreamReqTrailers != nil {
		s.upstreamRequest.appendTrailers()
	}

	// setup per try timeout timer
	s.setupPerReqTimeout()

	s.upstreamRequestSent = true
	s.downstreamRecvDone = true
}

// Downstream got reset in proxy context on scenario below:
// 1. downstream filter reset downstream
// 2. corresponding upstream got reset
func (s *downStream) resetStream() {
	if s.responseSender != nil && !s.upstreamProcessDone {
		// if downstream req received not done, or local proxy process not done by handle upstream response,
		// just mark it as done and reset stream as a failed case
		s.upstreamProcessDone = true

		// reset downstream will trigger a clean up, see OnResetStream
		s.responseSender.GetStream().ResetStream(types.StreamLocalReset)
	}
}

func (s *downStream) sendHijackReply(code int, headers types.HeaderMap) {
	s.logger.Errorf("set hijack reply, conn = %d, id = %d, code = %d", s.proxy.readCallbacks.Connection().ID(), s.ID, code)
	if headers == nil {
		s.logger.Warnf("hijack with no headers, conn = %d, id = %d", s.proxy.readCallbacks.Connection().ID(), s.ID)
		raw := make(map[string]string, 5)
		headers = protocol.CommonHeader(raw)
	}
	s.requestInfo.SetResponseCode(code)

	headers.Set(types.HeaderStatus, strconv.Itoa(code))
	atomic.StoreUint32(&s.reuseBuffer, 0)
	s.downstreamRespHeaders = headers
	s.downstreamRespDataBuf = nil
	s.downstreamRespTrailers = nil
	s.directResponse = true
}

// TODO: rpc status code may be not matched
// TODO: rpc content(body) is not matched the headers, rpc should not hijack with body, use sendHijackReply instead
func (s *downStream) sendHijackReplyWithBody(code int, headers types.HeaderMap, body string) {
	s.logger.Errorf("set hijack reply with body, conn = %d, stream id = %d, code = %d", s.proxy.readCallbacks.Connection().ID(), s.ID, code)
	if headers == nil {
		s.logger.Warnf("hijack with no headers, conn = %d, stream id = %d", s.proxy.readCallbacks.Connection().ID(), s.ID)
		raw := make(map[string]string, 5)
		headers = protocol.CommonHeader(raw)
	}
	s.requestInfo.SetResponseCode(code)
	headers.Set(types.HeaderStatus, strconv.Itoa(code))
	atomic.StoreUint32(&s.reuseBuffer, 0)
	s.downstreamRespHeaders = headers
	s.downstreamRespDataBuf = buffer.NewIoBufferString(body)
	s.downstreamRespTrailers = nil
	s.directResponse = true
}

func (s *downStream) cleanUp() {
	// reset retry state
	// if  a downstream filter ends downstream before send to upstream, retryState will be nil
	if s.retryState != nil {
		s.retryState.reset()
	}

	// reset pertry timer
	if s.perRetryTimer != nil {
		s.perRetryTimer.Stop()
		s.perRetryTimer = nil
	}

	// reset response timer
	if s.responseTimer != nil {
		s.responseTimer.Stop()
		s.responseTimer = nil
	}

}

func (s *downStream) setBufferLimit(bufferLimit uint32) {
	s.bufferLimit = bufferLimit

	// todo
}

func (s *downStream) AddStreamReceiverFilter(filter types.StreamReceiverFilter, p types.Phase) {
	sf := newActiveStreamReceiverFilter(s, filter, p)
	s.receiverFilters = append(s.receiverFilters, sf)
}

func (s *downStream) AddStreamSenderFilter(filter types.StreamSenderFilter) {
	sf := newActiveStreamSenderFilter(s, filter)
	s.senderFilters = append(s.senderFilters, sf)
}

func (s *downStream) AddStreamAccessLog(accessLog types.AccessLog) {
	if s.proxy != nil {
		if s.streamAccessLogs == nil {
			s.streamAccessLogs = make([]types.AccessLog, 0)
		}
		s.streamAccessLogs = append(s.streamAccessLogs, accessLog)
	}
}

// types.LoadBalancerContext
// no use currently
func (s *downStream) ComputeHashKey() types.HashedValue {
	//return [16]byte{}
	return ""
}

func (s *downStream) MetadataMatchCriteria() types.MetadataMatchCriteria {
	if nil != s.requestInfo.RouteEntry() {
		return s.requestInfo.RouteEntry().MetadataMatchCriteria(s.cluster.Name())
	}

	return nil
}

func (s *downStream) DownstreamConnection() net.Conn {
	return s.proxy.readCallbacks.Connection().RawConn()
}

func (s *downStream) DownstreamHeaders() types.HeaderMap {
	return s.downstreamReqHeaders
}

func (s *downStream) giveStream() {
	if s.snapshot != nil {
		s.proxy.clusterManager.PutClusterSnapshot(s.snapshot)
	}
	if atomic.LoadUint32(&s.reuseBuffer) != 1 {
		return
	}
	if atomic.LoadUint32(&s.upstreamReset) == 1 || atomic.LoadUint32(&s.downstreamReset) == 1 {
		return
	}

	s.logger.Debugf("downStream giveStream %p %+v", s, s)

	// reset downstreamReqBuf
	if s.downstreamReqDataBuf != nil {
		if e := buffer.PutIoBuffer(s.downstreamReqDataBuf); e != nil {
			s.logger.Errorf("PutIoBuffer error: %v", e)
		}
	}

	// Give buffers to bufferPool
	if ctx := buffer.PoolContext(s.context); ctx != nil {
		ctx.Give()
	}
}

// check if proxy process done
func (s *downStream) processDone() bool {
	return s.upstreamProcessDone || atomic.LoadUint32(&s.downstreamReset) == 1 || atomic.LoadUint32(&s.upstreamReset) == 1
}

func (s *downStream) sendNotify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *downStream) cleanNotify() {
	select {
	case <-s.notify:
	default:
	}
}

func (s *downStream) waitNotify(id uint32) (phase types.Phase, err error) {
	if s.ID != id {
		return types.End, types.ErrExit
	}

	s.logger.Debugf("waitNotify begin %p %d", s, s.ID)
	select {
	case <-s.notify:
	}
	return s.processError(id)
}

func (s *downStream) processError(id uint32) (phase types.Phase, err error) {
	if s.ID != id {
		return types.End, types.ErrExit
	}

	phase = types.End

	if atomic.LoadUint32(&s.downstreamCleaned) == 1 {
		err = types.ErrExit
		return
	}

	if atomic.LoadUint32(&s.upstreamReset) == 1 {
		s.logger.Errorf("processError upstreamReset downStream id: %d", s.ID)
		s.onUpstreamReset(s.resetReason)
		err = types.ErrExit
	}

	if atomic.LoadUint32(&s.downstreamReset) == 1 {
		s.logger.Errorf("processError downstreamReset downStream id: %d", s.ID)
		s.ResetStream(s.resetReason)
		err = types.ErrExit
		return
	}

	if s.directResponse {
		s.directResponse = false
		phase = types.UpFilter
		err = types.ErrExit
		return
	}

	if s.upstreamProcessDone {
		err = types.ErrExit
	}

	if s.upstreamRequest != nil && s.upstreamRequest.setupRetry {
		phase = types.Retry
		err = types.ErrExit
		return
	}

	if s.receiverFiltersAgain {
		s.receiverFiltersAgain = false
		phase = types.MatchRoute
		err = types.ErrExit
		return
	}

	return
}
