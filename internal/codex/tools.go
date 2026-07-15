package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/dpemmons/intercom/internal/appserver"
	"github.com/dpemmons/intercom/internal/intercomtools"
	"github.com/dpemmons/intercom/internal/wire"
)

type brokerTools interface {
	Send(context.Context, string, string) (wire.SendAck, error)
	ListPeers(context.Context) ([]string, error)
}

type reverseResponder interface {
	Respond(context.Context, any) error
	RespondError(context.Context, *appserver.RPCError) error
}

type reverseAuthorizer func(context.Context, string, string) error

type codexMCPMetadata struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		TurnID    string `json:"turn_id"`
	} `json:"x-codex-turn-metadata"`
}

type reverseHandler struct {
	broker     brokerTools
	authorize  reverseAuthorizer
	onOutbound func()
	onActivity func()
	onFatal    func(error)
	timeout    time.Duration
	logger     *slog.Logger
}

func dynamicToolSpecs() []appserver.DynamicToolSpec {
	return []appserver.DynamicToolSpec{
		{
			Type:        "function",
			Name:        intercomtools.SendMessageName,
			Description: intercomtools.SendMessageDescription,
			InputSchema: intercomtools.SendMessageInputSchema,
		},
		{
			Type:        "function",
			Name:        intercomtools.ListPeersName,
			Description: intercomtools.ListPeersDescription,
			InputSchema: intercomtools.ListPeersInputSchema,
		},
	}
}

func (h *reverseHandler) Handle(req *appserver.ReverseRequest) {
	if h.logger == nil {
		h.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	timeout := h.timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	workTimeout, responseTimeout := splitReverseBudget(timeout)
	ctx, cancel := context.WithTimeout(context.Background(), workTimeout)
	defer cancel()
	responder := freshResponseResponder{next: req, timeout: responseTimeout}
	if h.onActivity != nil {
		h.onActivity()
	}
	if err := h.handle(ctx, req.Method, req.Params, responder); err != nil {
		h.logger.Warn("answer app-server reverse request", "method", req.Method, "err", err)
		if h.onFatal != nil {
			h.onFatal(err)
		}
	}
}

func splitReverseBudget(total time.Duration) (work, response time.Duration) {
	response = total / 10
	if response > time.Second {
		response = time.Second
	}
	if response <= 0 {
		response = total / 2
	}
	work = total - response
	if work <= 0 {
		work = total / 2
		response = total - work
	}
	return work, response
}

type freshResponseResponder struct {
	next    reverseResponder
	timeout time.Duration
}

func (r freshResponseResponder) Respond(_ context.Context, result any) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	return r.next.Respond(ctx, result)
}

func (r freshResponseResponder) RespondError(_ context.Context, rpcErr *appserver.RPCError) error {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()
	return r.next.RespondError(ctx, rpcErr)
}

func (h *reverseHandler) handle(ctx context.Context, method string, params json.RawMessage, responder reverseResponder) error {
	switch method {
	case appserver.MethodDynamicToolCall:
		return h.handleDynamicTool(ctx, params, responder)
	case appserver.MethodCommandExecutionApproval:
		return respondDecoded[appserver.CommandExecutionRequestApprovalParams](ctx, params, responder,
			appserver.CommandExecutionRequestApprovalResponse{Decision: "decline"})
	case appserver.MethodFileChangeApproval:
		return respondDecoded[appserver.FileChangeRequestApprovalParams](ctx, params, responder,
			appserver.FileChangeRequestApprovalResponse{Decision: "decline"})
	case appserver.MethodPermissionsApproval:
		return respondDecoded[appserver.PermissionsRequestApprovalParams](ctx, params, responder,
			appserver.PermissionsRequestApprovalResponse{Permissions: appserver.GrantedPermissionProfile{}, Scope: "turn"})
	case appserver.MethodToolRequestUserInput:
		if err := decodeParams[appserver.ToolRequestUserInputParams](params); err != nil {
			return responder.RespondError(ctx, invalidParams(err))
		}
		return responder.RespondError(ctx, &appserver.RPCError{Code: appserver.ErrorCodeInternal, Message: "headless Intercom peer cannot answer user input"})
	case appserver.MethodMCPServerElicitation:
		if err := decodeParams[appserver.MCPServerElicitationRequestParams](params); err != nil {
			return responder.RespondError(ctx, invalidParams(err))
		}
		return responder.Respond(ctx, appserver.MCPServerElicitationRequestResponse{
			Action: "decline", Content: json.RawMessage("null"), Meta: json.RawMessage("null"),
		})
	case appserver.MethodLegacyApplyPatchApproval:
		return respondDecoded[appserver.LegacyApplyPatchApprovalParams](ctx, params, responder,
			appserver.LegacyApprovalResponse{Decision: "denied"})
	case appserver.MethodLegacyExecApproval:
		return respondDecoded[appserver.LegacyExecCommandApprovalParams](ctx, params, responder,
			appserver.LegacyApprovalResponse{Decision: "denied"})
	case appserver.MethodChatGPTAuthTokensRefresh,
		appserver.MethodAttestationGenerate,
		appserver.MethodCurrentTimeRead:
		return responder.RespondError(ctx, &appserver.RPCError{
			Code: appserver.ErrorCodeInternal, Message: "request is unavailable in a headless Intercom peer",
		})
	default:
		return responder.RespondError(ctx, &appserver.RPCError{
			Code: appserver.ErrorCodeMethodNotFound, Message: "unsupported app-server request: " + method,
		})
	}
}

func (h *reverseHandler) handleDynamicTool(ctx context.Context, raw json.RawMessage, responder reverseResponder) error {
	var params appserver.DynamicToolCallParams
	if err := json.Unmarshal(raw, &params); err != nil {
		return responder.RespondError(ctx, invalidParams(err))
	}
	if params.Namespace != nil {
		return respondDynamicViolation(ctx, responder, "Intercom dynamic tools must not carry a namespace")
	}
	if params.ThreadID == "" || params.TurnID == "" || params.CallID == "" {
		return respondDynamicViolation(ctx, responder, "dynamic tool call is missing threadId, turnId, or callId")
	}
	if h.authorize == nil {
		return responder.Respond(ctx, dynamicFailure("adapter is not ready"))
	}
	if err := h.authorize(ctx, params.ThreadID, params.TurnID); err != nil {
		if respondErr := responder.Respond(ctx, dynamicFailure(err.Error())); respondErr != nil {
			return respondErr
		}
		return fmt.Errorf("reject pre-ready or mismatched dynamic tool call: %w", err)
	}
	if h.broker == nil {
		return responder.Respond(ctx, dynamicFailure("Intercom broker is unavailable"))
	}

	switch params.Tool {
	case intercomtools.SendMessageName:
		args, err := intercomtools.DecodeSendMessage(params.Arguments)
		if err != nil {
			return responder.Respond(ctx, dynamicFailure(err.Error()))
		}
		ack, err := h.broker.Send(ctx, args.To, args.Message)
		if err != nil {
			return responder.Respond(ctx, dynamicFailure(intercomtools.SendFailed(err)))
		}
		if !ack.OK {
			return responder.Respond(ctx, dynamicFailure(intercomtools.SendRejected(ack.Code, ack.Message)))
		}
		if h.onOutbound != nil {
			h.onOutbound()
		}
		return responder.Respond(ctx, dynamicSuccess(intercomtools.SendAccepted(args.To)))
	case intercomtools.ListPeersName:
		if err := intercomtools.DecodeListPeers(params.Arguments); err != nil {
			return responder.Respond(ctx, dynamicFailure(err.Error()))
		}
		peers, err := h.broker.ListPeers(ctx)
		if err != nil {
			return responder.Respond(ctx, dynamicFailure(intercomtools.ListPeersFailed(err)))
		}
		return responder.Respond(ctx, dynamicSuccess(intercomtools.FormatPeers(peers)))
	default:
		return responder.Respond(ctx, dynamicFailure("unknown Intercom tool: "+params.Tool))
	}
}

func (c *controller) authorizeMCPMetadata(ctx context.Context, raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return errors.New("MCP tool call is missing Codex routing metadata")
	}
	var metadata codexMCPMetadata
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return fmt.Errorf("decode Codex MCP routing metadata: %w", err)
	}
	if metadata.ThreadID == "" || metadata.Turn.ThreadID == "" || metadata.Turn.TurnID == "" || metadata.Turn.SessionID == "" {
		return errors.New("MCP tool call metadata is missing threadId, session_id, thread_id, or turn_id")
	}
	if metadata.ThreadID != metadata.Turn.ThreadID {
		return fmt.Errorf("MCP tool call metadata disagrees on thread id %q/%q", metadata.ThreadID, metadata.Turn.ThreadID)
	}
	rootThreadID := c.managedThreadID()
	if metadata.Turn.SessionID != rootThreadID {
		return fmt.Errorf("MCP tool call session id %q does not match managed root %q", metadata.Turn.SessionID, rootThreadID)
	}
	return c.authorizeReverse(ctx, metadata.ThreadID, metadata.Turn.TurnID)
}

func (c *controller) bridgeSendMessage(ctx context.Context, metadata json.RawMessage, to, message string) (wire.SendAck, error) {
	c.touchActivity()
	if err := c.authorizeMCPMetadata(ctx, metadata); err != nil {
		wrapped := fmt.Errorf("reject pre-ready or mismatched MCP send_message: %w", err)
		c.signalFatal(wrapped)
		return wire.SendAck{}, wrapped
	}
	ack, err := c.broker.Send(ctx, to, message)
	if err != nil {
		return wire.SendAck{}, err
	}
	if ack.OK {
		c.noteOutbound()
	}
	return ack, nil
}

func (c *controller) bridgeListPeers(ctx context.Context, metadata json.RawMessage) ([]string, error) {
	c.touchActivity()
	if err := c.authorizeMCPMetadata(ctx, metadata); err != nil {
		wrapped := fmt.Errorf("reject pre-ready or mismatched MCP list_peers: %w", err)
		c.signalFatal(wrapped)
		return nil, wrapped
	}
	return c.broker.ListPeers(ctx)
}

func respondDynamicViolation(ctx context.Context, responder reverseResponder, message string) error {
	if err := responder.Respond(ctx, dynamicFailure(message)); err != nil {
		return err
	}
	return fmt.Errorf("reject invalid dynamic tool routing: %s", message)
}

func dynamicSuccess(text string) appserver.DynamicToolCallResponse {
	return appserver.DynamicToolCallResponse{Success: true, ContentItems: []appserver.DynamicToolCallOutputContentItem{appserver.DynamicToolText(text)}}
}

func dynamicFailure(text string) appserver.DynamicToolCallResponse {
	return appserver.DynamicToolCallResponse{Success: false, ContentItems: []appserver.DynamicToolCallOutputContentItem{appserver.DynamicToolText(text)}}
}

func invalidParams(err error) *appserver.RPCError {
	return &appserver.RPCError{Code: appserver.ErrorCodeInvalidParams, Message: err.Error()}
}

func decodeParams[T any](raw json.RawMessage) error {
	var value T
	return json.Unmarshal(raw, &value)
}

func respondDecoded[T any](ctx context.Context, raw json.RawMessage, responder reverseResponder, response any) error {
	if err := decodeParams[T](raw); err != nil {
		return responder.RespondError(ctx, invalidParams(err))
	}
	return responder.Respond(ctx, response)
}
