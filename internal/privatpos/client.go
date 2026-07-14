package privatpos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Stage marks the last significant point reached by Purchase.
type Stage string

const (
	StageConnect       Stage = "connect"
	StageHandshake     Stage = "handshake"
	StageIdentify      Stage = "identify"
	StageBeforeSend    Stage = "before_send"
	StageWriteRequest  Stage = "write_request"
	StageAwaitResponse Stage = "await_response"
	StageCompleted     Stage = "completed"
	StageClosed        Stage = "closed"
)

var errClientClosed = errors.New("privatpos: client closed")

const maxTerminalFrameSize = 1 << 20

// PurchaseOutcome reports the transport-side status of a purchase call.
type PurchaseOutcome struct {
	Response    *Response
	RequestSent bool
	Stage       Stage
}

// Client is a serialized direct TCP client for PrivatBank POS terminals.
type Client struct {
	address          string
	connectTimeout   time.Duration
	operationTimeout time.Duration

	opMu   chan struct{}
	mu     sync.Mutex
	state  *connectionState
	closed bool
}

type connectionState struct {
	conn   net.Conn
	frames chan Response

	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}
	doneOnce sync.Once

	mu  sync.Mutex
	err error
}

// NewClient builds a direct TCP client.
func NewClient(address string, connectTimeout, operationTimeout time.Duration) *Client {
	return &Client{
		address:          address,
		connectTimeout:   connectTimeout,
		operationTimeout: operationTimeout,
		opMu:             make(chan struct{}, 1),
	}
}

// Close stops the client and closes the persistent connection.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	state := c.state
	c.state = nil
	c.mu.Unlock()

	if state != nil {
		state.close()
	}
	return nil
}

// Purchase runs one serialized Purchase request.
func (c *Client) Purchase(ctx context.Context, amount, merchantID string, beforeSend func() error) (PurchaseOutcome, error) {
	outcome := PurchaseOutcome{Stage: StageConnect}

	select {
	case <-ctx.Done():
		return outcome, ctx.Err()
	case c.opMu <- struct{}{}:
	}
	defer func() { <-c.opMu }()
	if err := ctx.Err(); err != nil {
		return outcome, err
	}

	state, stage, err := c.ensureReady(ctx)
	if err != nil {
		outcome.Stage = stage
		return outcome, err
	}

	frame, err := marshalFrame(purchaseRequest(amount, merchantID), false)
	if err != nil {
		outcome.Stage = StageWriteRequest
		return outcome, fmt.Errorf("marshal purchase request: %w", err)
	}

	deadline := time.Now().Add(c.operationTimeout)
	if err := state.conn.SetReadDeadline(deadline); err != nil {
		outcome.Stage = StageAwaitResponse
		c.discard(state)
		return outcome, fmt.Errorf("set purchase read deadline: %w", err)
	}
	if err := state.conn.SetWriteDeadline(deadline); err != nil {
		outcome.Stage = StageWriteRequest
		c.discard(state)
		return outcome, fmt.Errorf("set purchase write deadline: %w", err)
	}
	defer func() {
		if err := clearDeadlines(state.conn); err != nil {
			c.discard(state)
		}
	}()

	outcome.Stage = StageWriteRequest
	if beforeSend != nil {
		if err := beforeSend(); err != nil {
			outcome.Stage = StageBeforeSend
			return outcome, fmt.Errorf("before sending purchase: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		outcome.Stage = StageBeforeSend
		return outcome, err
	}
	stopWatch := watchContext(ctx, state.conn)
	defer stopWatch()

	wrote, err := writeAll(state.conn, frame)
	if wrote > 0 {
		outcome.RequestSent = true
	}
	if err != nil {
		if ctx.Err() != nil {
			c.discard(state)
			return outcome, ctx.Err()
		}
		c.discard(state)
		return outcome, fmt.Errorf("write purchase request: %w", err)
	}
	if err := ctx.Err(); err != nil {
		c.discard(state)
		return outcome, err
	}

	outcome.Stage = StageAwaitResponse
	for {
		select {
		case response := <-state.frames:
			if err := ctx.Err(); err != nil {
				c.discard(state)
				return outcome, err
			}
			if response.Method == methodService {
				continue
			}
			if response.Method != methodPurchase {
				c.discard(state)
				return outcome, fmt.Errorf("unexpected response method %q", response.Method)
			}
			outcome.Response = &response
			outcome.Stage = StageCompleted
			return outcome, nil
		case <-state.done:
			if err := ctx.Err(); err != nil {
				c.discard(state)
				return outcome, err
			}
			if response, ok := state.tryFrame(); ok {
				if err := ctx.Err(); err != nil {
					c.discard(state)
					return outcome, err
				}
				if response.Method == methodService {
					continue
				}
				if response.Method != methodPurchase {
					c.discard(state)
					return outcome, fmt.Errorf("unexpected response method %q", response.Method)
				}
				outcome.Response = &response
				outcome.Stage = StageCompleted
				return outcome, nil
			}
			err := state.connectionErr()
			c.discard(state)
			return outcome, fmt.Errorf("await purchase response: %w", err)
		case <-ctx.Done():
			c.discard(state)
			return outcome, ctx.Err()
		}
	}
}

func (c *Client) ensureReady(ctx context.Context) (*connectionState, Stage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, StageClosed, errClientClosed
	}
	if c.state != nil && !c.state.isDone() {
		state := c.state
		c.mu.Unlock()
		return state, StageConnect, nil
	}
	stale := c.state
	c.state = nil
	c.mu.Unlock()
	if stale != nil {
		stale.close()
	}

	if err := c.exchangeVerify(ctx, handshakeRequest(), true, verifyHandshake); err != nil {
		return nil, StageHandshake, err
	}
	if err := c.exchangeVerify(ctx, identifyRequest(), false, verifyIdentify); err != nil {
		return nil, StageIdentify, err
	}

	conn, err := (&net.Dialer{Timeout: c.connectTimeout}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		return nil, StageConnect, fmt.Errorf("connect persistent socket: %w", err)
	}

	state := &connectionState{
		conn:   conn,
		frames: make(chan Response, 1),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go state.readLoop()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		state.close()
		return nil, StageClosed, errClientClosed
	}
	c.state = state
	c.mu.Unlock()
	return state, StageConnect, nil
}

func (c *Client) exchangeVerify(ctx context.Context, request Request, leadingNull bool, verify func(Response) error) error {
	conn, err := (&net.Dialer{Timeout: c.connectTimeout}).DialContext(ctx, "tcp", c.address)
	if err != nil {
		return fmt.Errorf("dial %s: %w", request.Method, err)
	}
	defer conn.Close()
	stopWatch := watchContext(ctx, conn)
	defer stopWatch()

	if err := conn.SetDeadline(time.Now().Add(c.operationTimeout)); err != nil {
		return fmt.Errorf("set %s deadline: %w", request.Method, err)
	}

	frame, err := marshalFrame(request, leadingNull)
	if err != nil {
		return fmt.Errorf("marshal %s request: %w", request.Method, err)
	}
	if _, err := writeAll(conn, frame); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("write %s request: %w", request.Method, err)
	}

	response, err := readOneFrame(conn)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("read %s response: %w", request.Method, err)
	}
	if err := verify(response); err != nil {
		return fmt.Errorf("%s verification failed: %w", request.Method, err)
	}
	return nil
}

func (c *Client) discard(state *connectionState) {
	c.mu.Lock()
	if c.state == state {
		c.state = nil
	}
	c.mu.Unlock()
	state.close()
}

func watchContext(ctx context.Context, conn net.Conn) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

func verifyHandshake(response Response) error {
	if response.Method != methodPingDevice {
		return fmt.Errorf("method = %q, want %q", response.Method, methodPingDevice)
	}
	if response.Error {
		return fmt.Errorf("terminal returned error: %s", response.ErrorDescription)
	}
	if value := response.paramString(paramResponseCode); value != "" && value != "0000" {
		return fmt.Errorf("responseCode = %q, want %q", value, "0000")
	}
	if value := response.paramString(paramCode); value != "" && value != "00" {
		return fmt.Errorf("code = %q, want %q", value, "00")
	}
	return nil
}

func verifyIdentify(response Response) error {
	if response.Method != methodService {
		return fmt.Errorf("method = %q, want %q", response.Method, methodService)
	}
	if response.Error {
		return fmt.Errorf("terminal returned error: %s", response.ErrorDescription)
	}
	if response.paramString(paramMsgType) != serviceIdentify {
		return fmt.Errorf("msgType = %q, want %q", response.paramString(paramMsgType), serviceIdentify)
	}
	if response.paramString(paramResult) != "true" {
		return fmt.Errorf("result = %q, want %q", response.paramString(paramResult), "true")
	}
	return nil
}

func marshalFrame(request Request, leadingNull bool) ([]byte, error) {
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if leadingNull {
		payload = append([]byte{0x00}, payload...)
	}
	return append(payload, 0x00), nil
}

func readOneFrame(r io.Reader) (Response, error) {
	var buf bytes.Buffer
	var chunk [512]byte
	for {
		n, err := r.Read(chunk[:])
		if n > 0 {
			buf.Write(chunk[:n])
			frame, ok, frameErr := popFrame(&buf)
			if frameErr != nil {
				return Response{}, frameErr
			}
			if ok {
				if len(frame) == 0 {
					continue
				}
				var response Response
				if err := json.Unmarshal(frame, &response); err != nil {
					return Response{}, fmt.Errorf("decode frame: %w", err)
				}
				return response, nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && buf.Len() == 0 {
				return Response{}, io.EOF
			}
			return Response{}, err
		}
	}
}

func popFrame(buf *bytes.Buffer) ([]byte, bool, error) {
	data := buf.Bytes()
	idx := bytes.IndexByte(data, 0x00)
	if idx < 0 {
		if len(data) > maxTerminalFrameSize {
			return nil, false, fmt.Errorf("terminal frame exceeds %d bytes", maxTerminalFrameSize)
		}
		return nil, false, nil
	}
	if idx > maxTerminalFrameSize {
		return nil, false, fmt.Errorf("terminal frame exceeds %d bytes", maxTerminalFrameSize)
	}
	frame := append([]byte(nil), data[:idx]...)
	buf.Next(idx + 1)
	return frame, true, nil
}

func writeAll(conn net.Conn, frame []byte) (int, error) {
	total := 0
	for total < len(frame) {
		n, err := conn.Write(frame[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func clearDeadlines(conn net.Conn) error {
	return errors.Join(
		conn.SetReadDeadline(time.Time{}),
		conn.SetWriteDeadline(time.Time{}),
	)
}

func (s *connectionState) readLoop() {
	var buf bytes.Buffer
	var chunk [512]byte
	for {
		n, err := s.conn.Read(chunk[:])
		if n > 0 {
			buf.Write(chunk[:n])
			for {
				frame, ok, frameErr := popFrame(&buf)
				if frameErr != nil {
					s.fail(frameErr)
					return
				}
				if !ok {
					break
				}
				if len(frame) == 0 {
					continue
				}
				var response Response
				if err := json.Unmarshal(frame, &response); err != nil {
					s.fail(fmt.Errorf("decode response frame: %w", err))
					return
				}
				if response.Method == methodService {
					continue
				}
				select {
				case s.frames <- response:
				case <-s.stop:
					s.fail(errClientClosed)
					return
				}
			}
		}
		if err != nil {
			select {
			case <-s.stop:
				s.fail(errClientClosed)
				return
			default:
			}
			s.fail(err)
			return
		}
	}
}

func (s *connectionState) fail(err error) {
	if err == nil {
		err = io.EOF
	}
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() {
		close(s.done)
	})
}

func (s *connectionState) close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		_ = s.conn.Close()
	})
	<-s.done
}

func (s *connectionState) isDone() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func (s *connectionState) connectionErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return io.EOF
	}
	return s.err
}

func (s *connectionState) tryFrame() (Response, bool) {
	select {
	case response := <-s.frames:
		return response, true
	default:
		return Response{}, false
	}
}

func (r Response) paramString(key string) string {
	if r.Params == nil {
		return ""
	}
	value, ok := r.Params[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}
