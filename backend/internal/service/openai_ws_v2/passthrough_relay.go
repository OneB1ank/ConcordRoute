package openai_ws_v2

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/TokenFlux/TokenRouter/internal/pkg/openai"
	coderws "github.com/coder/websocket"
	"github.com/tidwall/gjson"
)

type FrameConn interface {
	ReadFrame(ctx context.Context) (coderws.MessageType, []byte, error)
	WriteFrame(ctx context.Context, msgType coderws.MessageType, payload []byte) error
	Close() error
}

type Usage struct {
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
	ImageOutputTokens        int
}

type RelayResult struct {
	RequestModel            string
	Usage                   Usage
	RequestID               string
	TerminalEventType       string
	TerminalResponseBody    []byte
	FirstTokenMs            *int
	SemanticFirstTokenMs    *int // 展示用首语义事件，与真实首内容分离。
	Duration                time.Duration
	ClientToUpstreamFrames  int64
	UpstreamToClientFrames  int64
	DroppedDownstreamFrames int64
}

type RelayTurnResult struct {
	RequestModel         string
	Usage                Usage
	RequestID            string
	TerminalEventType    string
	TerminalResponseBody []byte
	Duration             time.Duration
	FirstTokenMs         *int
	SemanticFirstTokenMs *int
}

type RelayExit struct {
	Stage           string
	Err             error
	Graceful        bool
	WroteDownstream bool
}

// ErrDropDownstreamFrame 表示当前上游帧已由适配器处理，不应继续转发给客户端。
// 中继保持运行，使同一轮次可在受限原地恢复重试后继续。
var ErrDropDownstreamFrame = errors.New("drop upstream frame before downstream write")

type RelayOptions struct {
	WriteTimeout                    time.Duration
	IdleTimeout                     time.Duration
	UpstreamDrainTimeout            time.Duration
	FirstMessageType                coderws.MessageType
	FirstMessageSent                bool
	StartClientAfterFirstDownstream bool
	OnUsageParseFailure             func(eventType string, usageRaw string)
	// OnUpstreamEvent 在每个上游文本事件解析出 type 后回调，由 service 层统一筛选 warning 事件。
	OnUpstreamEvent func(eventType string, payload []byte)
	// OnTurnSettled 先推进所有已识别终态的生命周期，不依赖响应 ID 或用量解析。
	// OnTurnComplete 随后仅对文本观测结果回调，保留二进制帧不纳入统计的约定。
	OnTurnSettled     func(msgType coderws.MessageType)
	OnTurnComplete    func(turn RelayTurnResult)
	BeforeWriteClient func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error
	BeforeClientWrite func(msgType coderws.MessageType, payload []byte)
	AfterClientWrite  func(msgType coderws.MessageType, payload []byte, writeErr error)
	BeforeRelayCancel func(exit RelayExit)
	ReadClientFrame   func(ctx context.Context, clientConn FrameConn) (coderws.MessageType, []byte, error)
	OnTrace           func(event RelayTraceEvent)
	Now               func() time.Time
}

type RelayTraceEvent struct {
	Stage           string
	Direction       string
	MessageType     string
	PayloadBytes    int
	Graceful        bool
	WroteDownstream bool
	Error           string
}

type relayState struct {
	// pendingTurns 覆盖请求已发送、尚未获得响应 ID 的空档；不参与首字计时。
	pendingTurns atomic.Int64
	// turnOutputState 高位为轮次代数，最低位为本轮已写出，避免上一轮迟到的写完成覆盖新轮次。
	turnOutputState      atomic.Uint64
	usage                Usage
	requestModel         string
	lastResponseID       string
	terminalEventType    string
	terminalResponseBody []byte
	firstTokenMs         *int
	semanticFirstTokenMs *int
	turnTimingByID       map[string]*relayTurnTiming
	activeTurn           *relayTurnTiming
}

type relayExitSignal struct {
	stage           string
	err             error
	graceful        bool
	wroteDownstream bool
}

type observedUpstreamEvent struct {
	terminal           bool
	eventType          string
	responseID         string
	responseBody       []byte
	usage              Usage
	duration           time.Duration
	firstToken         *int
	semanticFirstToken *int
}

type relayTurnTiming struct {
	startAt              time.Time
	firstTokenMs         *int
	semanticFirstTokenMs *int
}

func Relay(
	ctx context.Context,
	clientConn FrameConn,
	upstreamConn FrameConn,
	firstClientMessage []byte,
	options RelayOptions,
) (RelayResult, *RelayExit) {
	result := RelayResult{RequestModel: strings.TrimSpace(gjson.GetBytes(firstClientMessage, "model").String())}
	if clientConn == nil || upstreamConn == nil {
		return result, &RelayExit{Stage: "relay_init", Err: errors.New("relay connection is nil")}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	nowFn := options.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	writeTimeout := options.WriteTimeout
	if writeTimeout <= 0 {
		writeTimeout = 2 * time.Minute
	}
	drainTimeout := options.UpstreamDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = 1200 * time.Millisecond
	}
	firstMessageType := options.FirstMessageType
	if firstMessageType != coderws.MessageBinary {
		firstMessageType = coderws.MessageText
	}
	startAt := nowFn()
	state := &relayState{requestModel: result.RequestModel}
	if isClientResponseCreateFrame(firstMessageType, firstClientMessage) {
		state.beginPendingTurn()
	}
	onTrace := options.OnTrace

	relayCtx, relayCancel := context.WithCancel(ctx)
	defer relayCancel()

	lastActivity := atomic.Int64{}
	lastActivity.Store(nowFn().UnixNano())
	markActivity := func() {
		lastActivity.Store(nowFn().UnixNano())
	}

	writeUpstream := func(msgType coderws.MessageType, payload []byte) error {
		writeCtx, cancel := context.WithTimeout(relayCtx, writeTimeout)
		defer cancel()
		return upstreamConn.WriteFrame(writeCtx, msgType, payload)
	}
	writeNextTurnUpstream := func(msgType coderws.MessageType, payload []byte) error {
		if isClientResponseCreateFrame(msgType, payload) {
			// 在写入前登记，保证上游立即应答时已经能观察到当前轮次。
			state.beginPendingTurn()
		}
		return writeUpstream(msgType, payload)
	}
	writeClient := func(msgType coderws.MessageType, payload []byte) error {
		// 下行写超时故意不挂在 relayCtx 上：coder/websocket 在已武装的 write
		// ctx 被取消时会直接硬关连接（context.AfterFunc 的 stop 不等待执行中
		// 的回调），外部取消若落在一次已成功写入的解除武装窗口内，会连同尚未
		// 发出的 close 帧一起冲掉，客户端只能看到裸 EOF 而收不到关闭码。与读
		// 侧 conn.Read(context.Background()) 同理，取消路径的连接回收由各退出
		// 分支的显式 Close/CloseNow 兜底。
		writeCtx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		return clientConn.WriteFrame(writeCtx, msgType, payload)
	}

	clientToUpstreamFrames := &atomic.Int64{}
	upstreamToClientFrames := &atomic.Int64{}
	droppedDownstreamFrames := &atomic.Int64{}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:        "relay_start",
		PayloadBytes: len(firstClientMessage),
		MessageType:  relayMessageTypeString(firstMessageType),
	})

	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_skipped",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	} else {
		if err := writeUpstream(firstMessageType, firstClientMessage); err != nil {
			result.Duration = nowFn().Sub(startAt)
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_first_message_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(firstMessageType),
				PayloadBytes: len(firstClientMessage),
				Error:        err.Error(),
			})
			return result, &RelayExit{Stage: "write_upstream", Err: err}
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:        "write_first_message_ok",
			Direction:    "client_to_upstream",
			MessageType:  relayMessageTypeString(firstMessageType),
			PayloadBytes: len(firstClientMessage),
		})
	}
	clientToUpstreamFrames.Add(1)
	markActivity()

	exitCh := make(chan relayExitSignal, 3)
	dropDownstreamWrites := atomic.Bool{}
	clientReaderStarted := atomic.Bool{}
	startClientReader := func() {
		if !clientReaderStarted.CompareAndSwap(false, true) {
			return
		}
		go runClientToUpstream(relayCtx, clientConn, options.ReadClientFrame, writeNextTurnUpstream, markActivity, clientToUpstreamFrames, onTrace, exitCh)
	}
	if !options.StartClientAfterFirstDownstream {
		startClientReader()
	}
	go runUpstreamToClient(
		relayCtx,
		upstreamConn,
		writeClient,
		startAt,
		nowFn,
		state,
		options.OnUsageParseFailure,
		options.OnUpstreamEvent,
		options.OnTurnSettled,
		options.OnTurnComplete,
		options.BeforeWriteClient,
		options.BeforeClientWrite,
		options.AfterClientWrite,
		func(msgType coderws.MessageType, payload []byte) {
			if options.StartClientAfterFirstDownstream {
				startClientReader()
			}
		},
		&dropDownstreamWrites,
		upstreamToClientFrames,
		droppedDownstreamFrames,
		markActivity,
		onTrace,
		exitCh,
	)
	go runIdleWatchdog(relayCtx, nowFn, options.IdleTimeout, &lastActivity, onTrace, exitCh)

	firstExit := <-exitCh
	// 外层入口取消属于控制面关闭，不是上游的正常断开。此处保持客户端连接，
	// 由适配器返回精确的租约或请求关闭码；内部 relayCancel 不会取消 ctx。
	if ctx.Err() != nil {
		firstExit.graceful = false
	}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "first_exit",
		Direction:       relayDirectionFromStage(firstExit.stage),
		Graceful:        firstExit.graceful,
		WroteDownstream: firstExit.wroteDownstream,
		Error:           relayErrorString(firstExit.err),
	})
	if options.BeforeRelayCancel != nil {
		options.BeforeRelayCancel(RelayExit{
			Stage:           firstExit.stage,
			Err:             firstExit.err,
			Graceful:        firstExit.graceful,
			WroteDownstream: firstExit.wroteDownstream,
		})
	}
	combinedWroteDownstream := firstExit.wroteDownstream
	secondExit := relayExitSignal{graceful: true}
	hasSecondExit := false

	// 客户端断开后尽力继续读取上游短窗口，捕获延迟 usage/terminal 事件用于计费。
	if firstExit.stage == "read_client" && firstExit.graceful {
		dropDownstreamWrites.Store(true)
		secondExit, hasSecondExit = waitRelayExit(exitCh, drainTimeout)
	} else {
		relayCancel()
		_ = upstreamConn.Close()
		if clientReaderStarted.Load() {
			secondExit, hasSecondExit = waitRelayExit(exitCh, 200*time.Millisecond)
		}
	}
	if hasSecondExit {
		combinedWroteDownstream = combinedWroteDownstream || secondExit.wroteDownstream
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "second_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        secondExit.graceful,
			WroteDownstream: secondExit.wroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
	}

	relayCancel()
	_ = upstreamConn.Close()

	enrichResult(&result, state, nowFn().Sub(startAt))
	result.ClientToUpstreamFrames = clientToUpstreamFrames.Load()
	result.UpstreamToClientFrames = upstreamToClientFrames.Load()
	result.DroppedDownstreamFrames = droppedDownstreamFrames.Load()
	if options.FirstMessageSent && firstExit.stage == "read_client" && firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	if firstExit.stage == "read_client" && firstExit.graceful {
		stage := "client_disconnected"
		exitErr := firstExit.err
		if hasSecondExit && !secondExit.graceful {
			stage = secondExit.stage
			exitErr = secondExit.err
		}
		if exitErr == nil {
			exitErr = io.EOF
		}
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(exitErr),
		})
		return result, &RelayExit{
			Stage:           stage,
			Err:             exitErr,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if firstExit.graceful && (!hasSecondExit || secondExit.graceful) {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_complete",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		_ = clientConn.Close()
		return result, nil
	}
	if !firstExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(firstExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(firstExit.err),
		})
		return result, &RelayExit{
			Stage:           firstExit.stage,
			Err:             firstExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if hasSecondExit && !secondExit.graceful {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_exit",
			Direction:       relayDirectionFromStage(secondExit.stage),
			Graceful:        false,
			WroteDownstream: combinedWroteDownstream,
			Error:           relayErrorString(secondExit.err),
		})
		return result, &RelayExit{
			Stage:           secondExit.stage,
			Err:             secondExit.err,
			WroteDownstream: combinedWroteDownstream,
		}
	}
	if options.FirstMessageSent {
		emitRelayTrace(onTrace, RelayTraceEvent{
			Stage:           "relay_client_closed",
			Graceful:        true,
			WroteDownstream: combinedWroteDownstream,
		})
		return result, nil
	}
	emitRelayTrace(onTrace, RelayTraceEvent{
		Stage:           "relay_complete",
		Graceful:        true,
		WroteDownstream: combinedWroteDownstream,
	})
	_ = clientConn.Close()
	return result, nil
}

func runClientToUpstream(
	ctx context.Context,
	clientConn FrameConn,
	readClientFrame func(context.Context, FrameConn) (coderws.MessageType, []byte, error),
	writeUpstream func(msgType coderws.MessageType, payload []byte) error,
	markActivity func(),
	forwardedFrames *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	if readClientFrame == nil {
		readClientFrame = func(ctx context.Context, conn FrameConn) (coderws.MessageType, []byte, error) {
			return conn.ReadFrame(ctx)
		}
	}
	for {
		msgType, payload, err := readClientFrame(ctx, clientConn)
		if err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "read_client_failed",
				Direction: "client_to_upstream",
				Error:     err.Error(),
				Graceful:  isDisconnectError(err),
			})
			exitCh <- relayExitSignal{stage: "read_client", err: err, graceful: isDisconnectError(err)}
			return
		}
		markActivity()
		if err := writeUpstream(msgType, payload); err != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:        "write_upstream_failed",
				Direction:    "client_to_upstream",
				MessageType:  relayMessageTypeString(msgType),
				PayloadBytes: len(payload),
				Error:        err.Error(),
			})
			exitCh <- relayExitSignal{stage: "write_upstream", err: err}
			return
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func runUpstreamToClient(
	ctx context.Context,
	upstreamConn FrameConn,
	writeClient func(msgType coderws.MessageType, payload []byte) error,
	startAt time.Time,
	nowFn func() time.Time,
	state *relayState,
	onUsageParseFailure func(eventType string, usageRaw string),
	onUpstreamEvent func(eventType string, payload []byte),
	onTurnSettled func(msgType coderws.MessageType),
	onTurnComplete func(turn RelayTurnResult),
	beforeWriteClient func(msgType coderws.MessageType, payload []byte, wroteDownstream bool) error,
	beforeClientWrite func(msgType coderws.MessageType, payload []byte),
	afterClientWrite func(msgType coderws.MessageType, payload []byte, writeErr error),
	afterWriteClient func(msgType coderws.MessageType, payload []byte),
	dropDownstreamWrites *atomic.Bool,
	forwardedFrames *atomic.Int64,
	droppedFrames *atomic.Int64,
	markActivity func(),
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	wroteDownstream := false
	for {
		msgType, payload, err := upstreamConn.ReadFrame(ctx)
		if err != nil {
			graceful := isDisconnectError(err)
			// 干净关闭只代表传输层完成关闭握手；如果当前已有未完成的
			// Responses turn，仍必须收到终止事件才算成功。
			if graceful && state.hasUnfinishedTurn() {
				graceful = false
				err = errors.New("upstream websocket closed before terminal event: " + err.Error())
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "read_upstream_failed",
				Direction:       "upstream_to_client",
				Error:           err.Error(),
				Graceful:        graceful,
				WroteDownstream: wroteDownstream,
			})
			exitCh <- relayExitSignal{
				stage:           "read_upstream",
				err:             err,
				graceful:        graceful,
				wroteDownstream: wroteDownstream,
			}
			return
		}
		markActivity()
		turnOutputState := uint64(0)
		if state != nil {
			turnOutputState = state.turnOutputState.Load()
		}
		if beforeWriteClient != nil {
			wroteInTurn := wroteDownstream
			if state != nil {
				wroteInTurn = turnOutputState&1 != 0
			}
			if err := beforeWriteClient(msgType, payload, wroteInTurn); err != nil {
				if errors.Is(err, ErrDropDownstreamFrame) {
					if droppedFrames != nil {
						droppedFrames.Add(1)
					}
					emitRelayTrace(onTrace, RelayTraceEvent{
						Stage:           "drop_downstream_frame",
						Direction:       "upstream_to_client",
						MessageType:     relayMessageTypeString(msgType),
						PayloadBytes:    len(payload),
						WroteDownstream: wroteDownstream,
					})
					markActivity()
					continue
				}
				emitRelayTrace(onTrace, RelayTraceEvent{
					Stage:           "upstream_message_rejected",
					Direction:       "upstream_to_client",
					MessageType:     relayMessageTypeString(msgType),
					PayloadBytes:    len(payload),
					WroteDownstream: wroteDownstream,
					Error:           err.Error(),
				})
				exitCh <- relayExitSignal{
					stage:           "upstream_message",
					err:             err,
					wroteDownstream: wroteDownstream,
				}
				return
			}
		}
		observedEvent := observedUpstreamEvent{}
		terminal := false
		switch msgType {
		case coderws.MessageText:
			observedEvent = observeUpstreamMessage(state, payload, startAt, nowFn, onUsageParseFailure, onUpstreamEvent)
			terminal = observedEvent.terminal
		case coderws.MessageBinary:
			// 二进制帧仍不解析用量，但其中明确的终止事件需要结算连接生命周期。
			terminal = isTerminalEvent(strings.TrimSpace(gjson.GetBytes(payload, "type").String()))
			if terminal {
				state.settleUnobservedTurn(strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()))
			}
		}
		if terminal && onTurnSettled != nil {
			onTurnSettled(msgType)
		}
		emitTurnComplete(onTurnComplete, state, observedEvent)
		if dropDownstreamWrites != nil && dropDownstreamWrites.Load() {
			if droppedFrames != nil {
				droppedFrames.Add(1)
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "drop_downstream_frame",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
			})
			if terminal {
				exitCh <- relayExitSignal{
					stage:           "drain_terminal",
					graceful:        true,
					wroteDownstream: wroteDownstream,
				}
				return
			}
			markActivity()
			continue
		}
		if beforeClientWrite != nil {
			beforeClientWrite(msgType, payload)
		}
		writeErr := writeClient(msgType, payload)
		if afterClientWrite != nil {
			afterClientWrite(msgType, payload, writeErr)
		}
		if writeErr != nil {
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:           "write_client_failed",
				Direction:       "upstream_to_client",
				MessageType:     relayMessageTypeString(msgType),
				PayloadBytes:    len(payload),
				WroteDownstream: wroteDownstream,
				Error:           writeErr.Error(),
			})
			exitCh <- relayExitSignal{stage: "write_client", err: writeErr, wroteDownstream: wroteDownstream}
			return
		}
		wroteDownstream = true
		if state != nil {
			// 下行写完成期间客户端可能已开始下一轮，仅更新写入所属的那一代。
			state.turnOutputState.CompareAndSwap(turnOutputState, turnOutputState|1)
		}
		if afterWriteClient != nil {
			afterWriteClient(msgType, payload)
		}
		if forwardedFrames != nil {
			forwardedFrames.Add(1)
		}
		markActivity()
	}
}

func runIdleWatchdog(
	ctx context.Context,
	nowFn func() time.Time,
	idleTimeout time.Duration,
	lastActivity *atomic.Int64,
	onTrace func(event RelayTraceEvent),
	exitCh chan<- relayExitSignal,
) {
	if idleTimeout <= 0 {
		return
	}
	checkInterval := minDuration(idleTimeout/4, 5*time.Second)
	if checkInterval < time.Second {
		checkInterval = time.Second
	}
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := time.Unix(0, lastActivity.Load())
			if nowFn().Sub(last) < idleTimeout {
				continue
			}
			emitRelayTrace(onTrace, RelayTraceEvent{
				Stage:     "idle_timeout_triggered",
				Direction: "watchdog",
				Error:     context.DeadlineExceeded.Error(),
			})
			exitCh <- relayExitSignal{stage: "idle_timeout", err: context.DeadlineExceeded}
			return
		}
	}
}

func emitRelayTrace(onTrace func(event RelayTraceEvent), event RelayTraceEvent) {
	if onTrace == nil {
		return
	}
	onTrace(event)
}

func relayMessageTypeString(msgType coderws.MessageType) string {
	switch msgType {
	case coderws.MessageText:
		return "text"
	case coderws.MessageBinary:
		return "binary"
	default:
		return "unknown(" + strconv.Itoa(int(msgType)) + ")"
	}
}

func relayDirectionFromStage(stage string) string {
	switch stage {
	case "read_client", "write_upstream":
		return "client_to_upstream"
	case "read_upstream", "write_client", "drain_terminal":
		return "upstream_to_client"
	case "idle_timeout":
		return "watchdog"
	default:
		return ""
	}
}

func relayErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func observeUpstreamMessage(
	state *relayState,
	message []byte,
	startAt time.Time,
	nowFn func() time.Time,
	onUsageParseFailure func(eventType string, usageRaw string),
	onUpstreamEvent func(eventType string, payload []byte),
) observedUpstreamEvent {
	if state == nil || len(message) == 0 {
		return observedUpstreamEvent{}
	}
	values := gjson.GetManyBytes(message, "type", "response.id", "response_id", "id")
	eventType := strings.TrimSpace(values[0].String())
	if eventType == "" {
		return observedUpstreamEvent{}
	}
	if onUpstreamEvent != nil {
		bodyCopy := append([]byte(nil), message...)
		onUpstreamEvent(eventType, bodyCopy)
	}
	responseID := strings.TrimSpace(values[1].String())
	if responseID == "" {
		responseID = strings.TrimSpace(values[2].String())
	}
	// 仅 terminal 事件兜底读取符合 OpenAI response ID 格式的顶层 id，
	// 避免把 evt_* 等 event ID 当成 response_id 关联到 turn。
	if responseID == "" && isTerminalEvent(eventType) {
		topLevelID := strings.TrimSpace(values[3].String())
		if strings.HasPrefix(topLevelID, "resp_") {
			responseID = topLevelID
		}
	}
	now := nowFn()
	if eventType == "error" && responseID == "" && state.activeTurn == nil {
		// 首响应前的明确拒绝不是无响应断流；仍透传错误，不合成成功终止事件。
		state.consumePendingTurn()
	}

	// 两种首事件分类仅用于统计；不修改透传帧、流控制或终止事件分类。
	// 每种样本记录后分别停止对应扫描，避免后续大帧重复付出分类开销。
	turnTiming := state.activeTurn
	if responseID != "" {
		turnTiming = state.turnTimingByID[responseID]
	}
	needsFirstToken := state.firstTokenMs == nil
	if turnTiming != nil && turnTiming.firstTokenMs == nil || responseID != "" && turnTiming == nil {
		needsFirstToken = true
	}
	visibleOutput := needsFirstToken && openai.StreamDataStartsVisibleOutputBytes(message, eventType)
	needsSemantic := state.semanticFirstTokenMs == nil ||
		(turnTiming != nil && turnTiming.semanticFirstTokenMs == nil) || (responseID != "" && turnTiming == nil)
	semanticOutput := needsSemantic && openai.StreamDataStartsSemanticOutputBytes(message, eventType)
	recordRelayFirstTokenMs(&state.firstTokenMs, startAt, now, visibleOutput)
	recordRelayFirstTokenMs(&state.semanticFirstTokenMs, startAt, now, semanticOutput)
	// 无 ID 内容使用活动轮次，不受连接级首字已经产生的影响。
	if turnTiming != nil {
		recordRelayFirstTokenMs(&turnTiming.firstTokenMs, turnTiming.startAt, now, visibleOutput)
		recordRelayFirstTokenMs(&turnTiming.semanticFirstTokenMs, turnTiming.startAt, now, semanticOutput)
	}
	parsedUsage := parseUsageAndAccumulate(state, message, eventType, onUsageParseFailure)
	observed := observedUpstreamEvent{
		eventType:  eventType,
		responseID: responseID,
		usage:      parsedUsage,
	}
	if responseID != "" {
		turnTiming := openAIWSRelayGetOrInitTurnTiming(state, responseID, now)
		if turnTiming != nil {
			recordRelayFirstTokenMs(&turnTiming.firstTokenMs, turnTiming.startAt, now, visibleOutput)
			recordRelayFirstTokenMs(&turnTiming.semanticFirstTokenMs, turnTiming.startAt, now, semanticOutput)
		}
	}
	if !isTerminalEvent(eventType) {
		return observed
	}
	observed.terminal = true
	observed.responseBody = terminalEventResponseBody(message)
	state.terminalEventType = eventType
	state.terminalResponseBody = cloneBytes(observed.responseBody)
	var completedTiming relayTurnTiming
	var timingFound bool
	if responseID != "" {
		state.lastResponseID = responseID
		completedTiming, timingFound = openAIWSRelayDeleteTurnTiming(state, responseID)
	} else {
		completedTiming, timingFound = state.settleUnobservedTurn("")
	}
	if timingFound {
		duration := now.Sub(completedTiming.startAt)
		if duration < 0 {
			duration = 0
		}
		observed.duration = duration
		observed.firstToken = openAIWSRelayCloneIntPtr(completedTiming.firstTokenMs)
		observed.semanticFirstToken = openAIWSRelayCloneIntPtr(completedTiming.semanticFirstTokenMs)
	}
	return observed
}

func emitTurnComplete(
	onTurnComplete func(turn RelayTurnResult),
	state *relayState,
	observed observedUpstreamEvent,
) {
	if onTurnComplete == nil || !observed.terminal {
		return
	}
	responseID := strings.TrimSpace(observed.responseID)
	// 无 ID 的文本终态也必须完成本轮观测；保留空值，不伪造上游响应 ID。
	requestModel := ""
	if state != nil {
		requestModel = state.requestModel
	}
	onTurnComplete(RelayTurnResult{
		RequestModel:         requestModel,
		Usage:                observed.usage,
		RequestID:            responseID,
		TerminalEventType:    observed.eventType,
		TerminalResponseBody: cloneBytes(observed.responseBody),
		Duration:             observed.duration,
		FirstTokenMs:         openAIWSRelayCloneIntPtr(observed.firstToken),
		SemanticFirstTokenMs: openAIWSRelayCloneIntPtr(observed.semanticFirstToken),
	})
}

func terminalEventResponseBody(message []byte) []byte {
	if len(message) == 0 {
		return nil
	}
	response := gjson.GetBytes(message, "response")
	if response.Exists() && response.Raw != "" {
		// terminal event 的 response 对象等价于普通 Responses 响应体。
		return []byte(response.Raw)
	}
	return cloneBytes(message)
}

func cloneBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte(nil), value...)
}

func openAIWSRelayGetOrInitTurnTiming(state *relayState, responseID string, now time.Time) *relayTurnTiming {
	if state == nil {
		return nil
	}
	if state.turnTimingByID == nil {
		state.turnTimingByID = make(map[string]*relayTurnTiming, 8)
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil || timing.startAt.IsZero() {
		state.consumePendingTurn()
		timing = &relayTurnTiming{startAt: now}
		state.turnTimingByID[responseID] = timing
		state.activeTurn = timing
		return timing
	}
	return timing
}

// isClientResponseCreateFrame 同时识别文本和二进制承载的创建请求，不改写帧内容。
func isClientResponseCreateFrame(msgType coderws.MessageType, payload []byte) bool {
	return (msgType == coderws.MessageText || msgType == coderws.MessageBinary) &&
		strings.TrimSpace(gjson.GetBytes(payload, "type").String()) == "response.create"
}

// beginPendingTurn 由客户端读侧调用，其状态通过原子变量交给上游读侧。
func (s *relayState) beginPendingTurn() {
	s.pendingTurns.Add(1)
	for {
		current := s.turnOutputState.Load()
		if s.turnOutputState.CompareAndSwap(current, (current&^1)+2) {
			return
		}
	}
}

// consumePendingTurn 每个新响应只消耗一次登记，避免把辅助事件误算为新轮次。
func (s *relayState) consumePendingTurn() {
	if s == nil {
		return
	}
	for {
		current := s.pendingTurns.Load()
		if current <= 0 || s.pendingTurns.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// hasUnfinishedTurn 仅由上游读侧查询；轮次映射仍保持单协程所有权。
func (s *relayState) hasUnfinishedTurn() bool {
	return s != nil && (s.pendingTurns.Load() > 0 || len(s.turnTimingByID) > 0)
}

// settleUnobservedTurn 结算无 ID 或二进制终态，并保留已观测的轮次计时供文本回调使用。
func (s *relayState) settleUnobservedTurn(responseID string) (relayTurnTiming, bool) {
	if s == nil {
		return relayTurnTiming{}, false
	}
	if responseID != "" {
		if timing, ok := openAIWSRelayDeleteTurnTiming(s, responseID); ok {
			return timing, true
		}
	} else if s.activeTurn != nil {
		for id, timing := range s.turnTimingByID {
			if timing == s.activeTurn {
				return openAIWSRelayDeleteTurnTiming(s, id)
			}
		}
	}
	s.consumePendingTurn()
	return relayTurnTiming{}, false
}

func openAIWSRelayDeleteTurnTiming(state *relayState, responseID string) (relayTurnTiming, bool) {
	if state == nil || state.turnTimingByID == nil {
		return relayTurnTiming{}, false
	}
	timing, ok := state.turnTimingByID[responseID]
	if !ok || timing == nil {
		return relayTurnTiming{}, false
	}
	delete(state.turnTimingByID, responseID)
	if state.activeTurn == timing {
		state.activeTurn = nil
	}
	return *timing, true
}

func openAIWSRelayCloneIntPtr(v *int) *int {
	if v == nil {
		return nil
	}
	cloned := *v
	return &cloned
}

// 两种统计共用只写一次和负值钳制，不改变轮次的起止/关联规则。
func recordRelayFirstTokenMs(value **int, started, observed time.Time, matches bool) {
	if *value == nil && matches {
		ms := int(observed.Sub(started).Milliseconds())
		if ms < 0 {
			ms = 0
		}
		*value = &ms
	}
}

func parseUsageAndAccumulate(
	state *relayState,
	message []byte,
	eventType string,
	onParseFailure func(eventType string, usageRaw string),
) Usage {
	if state == nil || len(message) == 0 || !shouldParseUsage(eventType) {
		return Usage{}
	}
	usageResult := gjson.GetBytes(message, "response.usage")
	if !usageResult.Exists() {
		return Usage{}
	}
	usageRaw := strings.TrimSpace(usageResult.Raw)
	if usageRaw == "" || !strings.HasPrefix(usageRaw, "{") {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		return Usage{}
	}

	inputResult := gjson.GetBytes(message, "response.usage.input_tokens")
	if !inputResult.Exists() {
		inputResult = gjson.GetBytes(message, "response.usage.prompt_tokens")
	}
	outputResult := gjson.GetBytes(message, "response.usage.output_tokens")
	if !outputResult.Exists() {
		outputResult = gjson.GetBytes(message, "response.usage.completion_tokens")
	}
	cachedResult := gjson.GetBytes(message, "response.usage.input_tokens_details.cached_tokens")
	if !cachedResult.Exists() {
		cachedResult = gjson.GetBytes(message, "response.usage.prompt_tokens_details.cached_tokens")
	}
	imageTokens := usageResult.Get("output_tokens_details.image_tokens").Int()
	if imageTokens == 0 {
		imageTokens = usageResult.Get("completion_tokens_details.image_tokens").Int()
	}

	inputTokens, inputOK := parseUsageIntField(inputResult, true)
	outputTokens, outputOK := parseUsageIntField(outputResult, true)
	cachedTokens, cachedOK := parseUsageIntField(cachedResult, false)
	if !inputOK || !outputOK || !cachedOK {
		recordUsageParseFailure()
		if onParseFailure != nil {
			onParseFailure(eventType, usageRaw)
		}
		// 解析失败时不做部分字段累加，避免计费 usage 出现“半有效”状态。
		return Usage{}
	}
	parsedUsage := Usage{
		InputTokens:              inputTokens,
		OutputTokens:             outputTokens,
		CacheCreationInputTokens: openAICacheCreationTokensFromUsage(usageResult),
		CacheReadInputTokens:     cachedTokens,
		ImageOutputTokens:        int(imageTokens),
	}

	state.usage.InputTokens += parsedUsage.InputTokens
	state.usage.OutputTokens += parsedUsage.OutputTokens
	state.usage.CacheCreationInputTokens += parsedUsage.CacheCreationInputTokens
	state.usage.CacheReadInputTokens += parsedUsage.CacheReadInputTokens
	state.usage.ImageOutputTokens += parsedUsage.ImageOutputTokens
	return parsedUsage
}

func parseUsageIntField(value gjson.Result, required bool) (int, bool) {
	if !value.Exists() {
		return 0, !required
	}
	if value.Type != gjson.Number {
		return 0, false
	}
	return int(value.Int()), true
}

func openAICacheCreationTokensFromUsage(value gjson.Result) int {
	for _, field := range []string{
		"input_tokens_details.cache_write_tokens",
		"prompt_tokens_details.cache_write_tokens",
		"input_tokens_details.cache_creation_tokens",
		"prompt_tokens_details.cache_creation_tokens",
	} {
		result := value.Get(field)
		if result.Exists() {
			return max(int(result.Int()), 0)
		}
	}
	for _, field := range []string{
		"cache_write_tokens",
		"cache_creation_input_tokens",
		"cache_write_input_tokens",
		"cache_creation_tokens",
	} {
		if tokens := int(value.Get(field).Int()); tokens > 0 {
			return tokens
		}
	}
	return 0
}

func enrichResult(result *RelayResult, state *relayState, duration time.Duration) {
	if result == nil {
		return
	}
	result.Duration = duration
	if state == nil {
		return
	}
	result.RequestModel = state.requestModel
	result.Usage = state.usage
	result.RequestID = state.lastResponseID
	result.TerminalEventType = state.terminalEventType
	result.TerminalResponseBody = cloneBytes(state.terminalResponseBody)
	result.FirstTokenMs = state.firstTokenMs
	result.SemanticFirstTokenMs = openAIWSRelayCloneIntPtr(state.semanticFirstTokenMs)
}

func isDisconnectError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		return true
	}
	switch coderws.CloseStatus(err) {
	case coderws.StatusNormalClosure, coderws.StatusGoingAway, coderws.StatusNoStatusRcvd, coderws.StatusAbnormalClosure:
		return true
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	if message == "" {
		return false
	}
	return strings.Contains(message, "failed to read frame header: eof") ||
		strings.Contains(message, "unexpected eof") ||
		strings.Contains(message, "use of closed network connection") ||
		strings.Contains(message, "connection reset by peer") ||
		strings.Contains(message, "broken pipe")
}

func isTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func shouldParseUsage(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
		return true
	default:
		return false
	}
}

func isTokenEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	return strings.HasSuffix(eventType, ".delta") ||
		eventType == "response.output_text.done" ||
		eventType == "response.function_call_arguments.done"
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if b <= 0 {
		return a
	}
	if a < b {
		return a
	}
	return b
}

func waitRelayExit(exitCh <-chan relayExitSignal, timeout time.Duration) (relayExitSignal, bool) {
	if timeout <= 0 {
		timeout = 200 * time.Millisecond
	}
	select {
	case sig := <-exitCh:
		return sig, true
	case <-time.After(timeout):
		return relayExitSignal{}, false
	}
}
