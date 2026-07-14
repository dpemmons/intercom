// Package appserver implements the small Codex app-server protocol surface
// used by Intercom. The wire shapes in this file are pinned to codex-cli
// 0.144.1's experimental generated schema.
//
// Codex app-server uses JSON-RPC-shaped messages without the "jsonrpc":"2.0"
// member. Unknown object fields are intentionally ignored by encoding/json so
// additive protocol changes remain forward compatible.
package appserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// ProtocolVersion is the Codex CLI version whose experimental schema these
// types implement.
const ProtocolVersion = "0.144.1"

// Method names used by the minimal managed-thread client.
const (
	MethodInitialize    = "initialize"
	MethodInitialized   = "initialized"
	MethodThreadStart   = "thread/start"
	MethodThreadResume  = "thread/resume"
	MethodThreadRead    = "thread/read"
	MethodTurnStart     = "turn/start"
	MethodTurnInterrupt = "turn/interrupt"
)

// Reverse request methods emitted by app-server 0.144.1.
const (
	MethodCommandExecutionApproval = "item/commandExecution/requestApproval"
	MethodFileChangeApproval       = "item/fileChange/requestApproval"
	MethodToolRequestUserInput     = "item/tool/requestUserInput"
	MethodMCPServerElicitation     = "mcpServer/elicitation/request"
	MethodPermissionsApproval      = "item/permissions/requestApproval"
	MethodDynamicToolCall          = "item/tool/call"
	MethodChatGPTAuthTokensRefresh = "account/chatgptAuthTokens/refresh"
	MethodAttestationGenerate      = "attestation/generate"
	MethodCurrentTimeRead          = "currentTime/read"
	MethodLegacyApplyPatchApproval = "applyPatchApproval"
	MethodLegacyExecApproval       = "execCommandApproval"
)

// Lifecycle notifications used by the managed-thread controller. Other
// notification methods remain available through Notification's raw payload.
const (
	NotificationError         = "error"
	NotificationThreadStarted = "thread/started"
	NotificationTurnStarted   = "turn/started"
	NotificationTurnCompleted = "turn/completed"
	NotificationItemStarted   = "item/started"
	NotificationItemCompleted = "item/completed"
)

const (
	ErrorCodeInvalidRequest = int64(-32600)
	ErrorCodeMethodNotFound = int64(-32601)
	ErrorCodeInvalidParams  = int64(-32602)
	ErrorCodeInternal       = int64(-32603)
)

type requestIDKind uint8

const (
	requestIDInvalid requestIDKind = iota
	requestIDNumber
	requestIDString
)

// RequestID is the protocol's string-or-signed-integer request identifier.
// It is comparable and can therefore be used directly as a map key.
type RequestID struct {
	kind   requestIDKind
	number int64
	text   string
}

func NumberRequestID(id int64) RequestID  { return RequestID{kind: requestIDNumber, number: id} }
func StringRequestID(id string) RequestID { return RequestID{kind: requestIDString, text: id} }

func (id RequestID) IsZero() bool { return id.kind == requestIDInvalid }

func (id RequestID) Number() (int64, bool) {
	return id.number, id.kind == requestIDNumber
}

func (id RequestID) Text() (string, bool) {
	return id.text, id.kind == requestIDString
}

func (id RequestID) String() string {
	switch id.kind {
	case requestIDNumber:
		return strconv.FormatInt(id.number, 10)
	case requestIDString:
		return id.text
	default:
		return "<invalid>"
	}
}

func (id RequestID) MarshalJSON() ([]byte, error) {
	switch id.kind {
	case requestIDNumber:
		return []byte(strconv.FormatInt(id.number, 10)), nil
	case requestIDString:
		return json.Marshal(id.text)
	default:
		return nil, errors.New("appserver: invalid request id")
	}
}

func (id *RequestID) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return errors.New("appserver: empty request id")
	}
	if data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return fmt.Errorf("appserver: decode string request id: %w", err)
		}
		*id = StringRequestID(text)
		return nil
	}
	number, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return fmt.Errorf("appserver: request id must be a string or signed integer: %w", err)
	}
	*id = NumberRequestID(number)
	return nil
}

// RPCError is an app-server error response. It also implements error.
type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("app-server RPC error %d: %s", e.Code, e.Message)
}

// Notification is a raw, forward-compatible app-server notification.
type Notification struct {
	Method string
	Params json.RawMessage
}

func (n Notification) DecodeParams(dst any) error {
	if len(n.Params) == 0 {
		return errors.New("appserver: notification has no params")
	}
	if err := json.Unmarshal(n.Params, dst); err != nil {
		return fmt.Errorf("appserver: decode %s notification: %w", n.Method, err)
	}
	return nil
}

type ClientInfo struct {
	Name    string  `json:"name"`
	Title   *string `json:"title"`
	Version string  `json:"version"`
}

type InitializeCapabilities struct {
	ExperimentalAPI                bool     `json:"experimentalApi"`
	RequestAttestation             bool     `json:"requestAttestation"`
	MCPServerOpenAIFormElicitation bool     `json:"mcpServerOpenaiFormElicitation,omitempty"`
	OptOutNotificationMethods      []string `json:"optOutNotificationMethods,omitempty"`
}

type InitializeParams struct {
	ClientInfo   ClientInfo              `json:"clientInfo"`
	Capabilities *InitializeCapabilities `json:"capabilities"`
}

type InitializeResponse struct {
	UserAgent      string `json:"userAgent"`
	CodexHome      string `json:"codexHome"`
	PlatformFamily string `json:"platformFamily"`
	PlatformOS     string `json:"platformOs"`
}

type SandboxMode string

const (
	SandboxReadOnly         SandboxMode = "read-only"
	SandboxWorkspaceWrite   SandboxMode = "workspace-write"
	SandboxDangerFullAccess SandboxMode = "danger-full-access"
)

type ApprovalPolicyName string

const (
	ApprovalUntrusted ApprovalPolicyName = "untrusted"
	ApprovalOnRequest ApprovalPolicyName = "on-request"
	ApprovalNever     ApprovalPolicyName = "never"
)

type ApprovalsReviewer string

const (
	ApprovalsReviewerUser       ApprovalsReviewer = "user"
	ApprovalsReviewerAutoReview ApprovalsReviewer = "auto_review"
	ApprovalsReviewerGuardian   ApprovalsReviewer = "guardian_subagent"
)

// SandboxPolicy is the superset of all four 0.144.1 sandbox-policy variants.
// Fields that do not apply to Type are omitted.
type SandboxPolicy struct {
	Type                string   `json:"type"`
	NetworkAccess       any      `json:"networkAccess,omitempty"`
	WritableRoots       []string `json:"writableRoots,omitempty"`
	ExcludeTmpdirEnvVar bool     `json:"excludeTmpdirEnvVar,omitempty"`
	ExcludeSlashTmp     bool     `json:"excludeSlashTmp,omitempty"`
}

type ActivePermissionProfile struct {
	ID      string  `json:"id"`
	Extends *string `json:"extends"`
}

// DynamicToolSpec is the flattened function-or-namespace dynamic tool union.
// Intercom uses Type "function".
type DynamicToolSpec struct {
	Type         string                     `json:"type"`
	Name         string                     `json:"name"`
	Description  string                     `json:"description"`
	InputSchema  any                        `json:"inputSchema,omitempty"`
	DeferLoading bool                       `json:"deferLoading,omitempty"`
	Tools        []DynamicToolNamespaceTool `json:"tools,omitempty"`
}

type DynamicToolNamespaceTool struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	InputSchema  any    `json:"inputSchema"`
	DeferLoading bool   `json:"deferLoading,omitempty"`
}

type ThreadStartParams struct {
	Model                 *string            `json:"model,omitempty"`
	ModelProvider         *string            `json:"modelProvider,omitempty"`
	CWD                   *string            `json:"cwd,omitempty"`
	RuntimeWorkspaceRoots []string           `json:"runtimeWorkspaceRoots,omitempty"`
	ApprovalPolicy        any                `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer     *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	Sandbox               *SandboxMode       `json:"sandbox,omitempty"`
	Config                map[string]any     `json:"config,omitempty"`
	BaseInstructions      *string            `json:"baseInstructions,omitempty"`
	DeveloperInstructions *string            `json:"developerInstructions,omitempty"`
	Ephemeral             *bool              `json:"ephemeral,omitempty"`
	DynamicTools          []DynamicToolSpec  `json:"dynamicTools,omitempty"`
}

type ThreadResumeParams struct {
	ThreadID              string             `json:"threadId"`
	Model                 *string            `json:"model,omitempty"`
	ModelProvider         *string            `json:"modelProvider,omitempty"`
	CWD                   *string            `json:"cwd,omitempty"`
	RuntimeWorkspaceRoots []string           `json:"runtimeWorkspaceRoots,omitempty"`
	ApprovalPolicy        any                `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer     *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	Sandbox               *SandboxMode       `json:"sandbox,omitempty"`
	Config                map[string]any     `json:"config,omitempty"`
	BaseInstructions      *string            `json:"baseInstructions,omitempty"`
	DeveloperInstructions *string            `json:"developerInstructions,omitempty"`
	ExcludeTurns          bool               `json:"excludeTurns,omitempty"`
}

type ThreadReadParams struct {
	ThreadID     string `json:"threadId"`
	IncludeTurns bool   `json:"includeTurns,omitempty"`
}

type ThreadStatus struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}

const (
	ThreadStatusNotLoaded   = "notLoaded"
	ThreadStatusIdle        = "idle"
	ThreadStatusSystemError = "systemError"
	ThreadStatusActive      = "active"
)

type Thread struct {
	ID             string          `json:"id"`
	Extra          json.RawMessage `json:"extra"`
	SessionID      string          `json:"sessionId"`
	ForkedFromID   *string         `json:"forkedFromId"`
	ParentThreadID *string         `json:"parentThreadId"`
	Preview        string          `json:"preview"`
	Ephemeral      bool            `json:"ephemeral"`
	HistoryMode    string          `json:"historyMode"`
	ModelProvider  string          `json:"modelProvider"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	RecencyAt      *int64          `json:"recencyAt"`
	Status         ThreadStatus    `json:"status"`
	Path           *string         `json:"path"`
	CWD            string          `json:"cwd"`
	CLIVersion     string          `json:"cliVersion"`
	Source         json.RawMessage `json:"source"`
	ThreadSource   json.RawMessage `json:"threadSource"`
	AgentNickname  *string         `json:"agentNickname"`
	AgentRole      *string         `json:"agentRole"`
	GitInfo        json.RawMessage `json:"gitInfo"`
	Name           *string         `json:"name"`
	Turns          []Turn          `json:"turns"`
}

// ThreadResponse contains the common settings returned by thread/start and
// thread/resume.
type ThreadResponse struct {
	Thread                  Thread                   `json:"thread"`
	Model                   string                   `json:"model"`
	ModelProvider           string                   `json:"modelProvider"`
	ServiceTier             *string                  `json:"serviceTier"`
	CWD                     string                   `json:"cwd"`
	RuntimeWorkspaceRoots   []string                 `json:"runtimeWorkspaceRoots"`
	InstructionSources      []string                 `json:"instructionSources"`
	ApprovalPolicy          any                      `json:"approvalPolicy"`
	ApprovalsReviewer       ApprovalsReviewer        `json:"approvalsReviewer"`
	Sandbox                 SandboxPolicy            `json:"sandbox"`
	ActivePermissionProfile *ActivePermissionProfile `json:"activePermissionProfile"`
	ReasoningEffort         *string                  `json:"reasoningEffort"`
	MultiAgentMode          any                      `json:"multiAgentMode"`
}

type ThreadStartResponse struct{ ThreadResponse }

type ThreadResumeResponse struct {
	ThreadResponse
	InitialTurnsPage json.RawMessage `json:"initialTurnsPage"`
}

type ThreadReadResponse struct {
	Thread Thread `json:"thread"`
}

type TextElement struct {
	ByteRange   ByteRange `json:"byteRange"`
	Placeholder *string   `json:"placeholder"`
}

type ByteRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// UserInput is the flattened input union. Text inputs must carry a
// text_elements array (snake case in the pinned schema).
type UserInput struct {
	Type         string        `json:"type"`
	Text         string        `json:"text,omitempty"`
	TextElements []TextElement `json:"text_elements"`
	URL          string        `json:"url,omitempty"`
	Path         string        `json:"path,omitempty"`
	Name         string        `json:"name,omitempty"`
	Detail       string        `json:"detail,omitempty"`
}

func TextInput(text string) UserInput {
	return UserInput{Type: "text", Text: text, TextElements: make([]TextElement, 0)}
}

type TurnStartParams struct {
	ThreadID              string             `json:"threadId"`
	ClientUserMessageID   *string            `json:"clientUserMessageId,omitempty"`
	Input                 []UserInput        `json:"input"`
	CWD                   *string            `json:"cwd,omitempty"`
	RuntimeWorkspaceRoots []string           `json:"runtimeWorkspaceRoots,omitempty"`
	ApprovalPolicy        any                `json:"approvalPolicy,omitempty"`
	ApprovalsReviewer     *ApprovalsReviewer `json:"approvalsReviewer,omitempty"`
	SandboxPolicy         *SandboxPolicy     `json:"sandboxPolicy,omitempty"`
	Permissions           *string            `json:"permissions,omitempty"`
	Model                 *string            `json:"model,omitempty"`
	OutputSchema          any                `json:"outputSchema,omitempty"`
}

type TurnStartResponse struct {
	Turn Turn `json:"turn"`
}

type TurnInterruptParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
}

type TurnInterruptResponse struct{}

type Turn struct {
	ID          string            `json:"id"`
	Items       []json.RawMessage `json:"items"`
	ItemsView   string            `json:"itemsView"`
	Status      string            `json:"status"`
	Error       *TurnError        `json:"error"`
	StartedAt   *int64            `json:"startedAt"`
	CompletedAt *int64            `json:"completedAt"`
	DurationMS  *int64            `json:"durationMs"`
}

const (
	TurnStatusCompleted   = "completed"
	TurnStatusInterrupted = "interrupted"
	TurnStatusFailed      = "failed"
	TurnStatusInProgress  = "inProgress"
)

type TurnError struct {
	Message           string          `json:"message"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
	AdditionalDetails *string         `json:"additionalDetails"`
}

type ThreadStartedNotification struct {
	Thread Thread `json:"thread"`
}

type TurnStartedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type TurnCompletedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     Turn   `json:"turn"`
}

type ErrorNotification struct {
	Error     TurnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
}

type ItemNotification struct {
	Item          json.RawMessage `json:"item"`
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	StartedAtMS   *int64          `json:"startedAtMs,omitempty"`
	CompletedAtMS *int64          `json:"completedAtMs,omitempty"`
}

type DynamicToolCallParams struct {
	ThreadID  string          `json:"threadId"`
	TurnID    string          `json:"turnId"`
	CallID    string          `json:"callId"`
	Namespace *string         `json:"namespace"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

type DynamicToolCallOutputContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"imageUrl,omitempty"`
}

func DynamicToolText(text string) DynamicToolCallOutputContentItem {
	return DynamicToolCallOutputContentItem{Type: "inputText", Text: text}
}

type DynamicToolCallResponse struct {
	ContentItems []DynamicToolCallOutputContentItem `json:"contentItems"`
	Success      bool                               `json:"success"`
}

// The approval request types retain large, evolving nested objects as raw JSON
// while typing the routing and denial fields Intercom needs.
type CommandExecutionRequestApprovalParams struct {
	ThreadID              string            `json:"threadId"`
	TurnID                string            `json:"turnId"`
	ItemID                string            `json:"itemId"`
	StartedAtMS           int64             `json:"startedAtMs"`
	ApprovalID            *string           `json:"approvalId,omitempty"`
	EnvironmentID         *string           `json:"environmentId"`
	Reason                *string           `json:"reason,omitempty"`
	Command               *string           `json:"command,omitempty"`
	CWD                   *string           `json:"cwd,omitempty"`
	CommandActions        json.RawMessage   `json:"commandActions,omitempty"`
	AdditionalPermissions json.RawMessage   `json:"additionalPermissions,omitempty"`
	AvailableDecisions    []json.RawMessage `json:"availableDecisions,omitempty"`
}

type CommandExecutionRequestApprovalResponse struct {
	Decision any `json:"decision"`
}

const (
	CommandExecutionDecisionAccept           = "accept"
	CommandExecutionDecisionAcceptForSession = "acceptForSession"
	CommandExecutionDecisionDecline          = "decline"
	CommandExecutionDecisionCancel           = "cancel"
)

type FileChangeRequestApprovalParams struct {
	ThreadID    string  `json:"threadId"`
	TurnID      string  `json:"turnId"`
	ItemID      string  `json:"itemId"`
	StartedAtMS int64   `json:"startedAtMs"`
	Reason      *string `json:"reason,omitempty"`
	GrantRoot   *string `json:"grantRoot,omitempty"`
}

type FileChangeRequestApprovalResponse struct {
	Decision string `json:"decision"`
}

const (
	FileChangeDecisionAccept           = "accept"
	FileChangeDecisionAcceptForSession = "acceptForSession"
	FileChangeDecisionDecline          = "decline"
	FileChangeDecisionCancel           = "cancel"
)

type ToolRequestUserInputParams struct {
	ThreadID         string            `json:"threadId"`
	TurnID           string            `json:"turnId"`
	ItemID           string            `json:"itemId"`
	Questions        []json.RawMessage `json:"questions"`
	AutoResolutionMS *uint64           `json:"autoResolutionMs"`
}

type ToolRequestUserInputAnswer struct {
	Answers []string `json:"answers"`
}

type ToolRequestUserInputResponse struct {
	Answers map[string]ToolRequestUserInputAnswer `json:"answers"`
}

type MCPServerElicitationRequestParams struct {
	ThreadID        string          `json:"threadId"`
	TurnID          *string         `json:"turnId"`
	ServerName      string          `json:"serverName"`
	Mode            string          `json:"mode"`
	Meta            json.RawMessage `json:"_meta"`
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema,omitempty"`
	URL             string          `json:"url,omitempty"`
	ElicitationID   string          `json:"elicitationId,omitempty"`
}

type MCPServerElicitationRequestResponse struct {
	Action  string          `json:"action"`
	Content json.RawMessage `json:"content"`
	Meta    json.RawMessage `json:"_meta"`
}

type PermissionsRequestApprovalParams struct {
	ThreadID      string          `json:"threadId"`
	TurnID        string          `json:"turnId"`
	ItemID        string          `json:"itemId"`
	EnvironmentID *string         `json:"environmentId"`
	StartedAtMS   int64           `json:"startedAtMs"`
	CWD           string          `json:"cwd"`
	Reason        *string         `json:"reason"`
	Permissions   json.RawMessage `json:"permissions"`
}

type GrantedPermissionProfile struct {
	Network    json.RawMessage `json:"network,omitempty"`
	FileSystem json.RawMessage `json:"fileSystem,omitempty"`
}

type PermissionsRequestApprovalResponse struct {
	Permissions      GrantedPermissionProfile `json:"permissions"`
	Scope            string                   `json:"scope"`
	StrictAutoReview bool                     `json:"strictAutoReview,omitempty"`
}

const (
	PermissionGrantScopeTurn    = "turn"
	PermissionGrantScopeSession = "session"
)

type ChatGPTAuthTokensRefreshParams struct {
	Reason            string  `json:"reason"`
	PreviousAccountID *string `json:"previousAccountId,omitempty"`
}

type ChatGPTAuthTokensRefreshResponse struct {
	AccessToken      string  `json:"accessToken"`
	ChatGPTAccountID string  `json:"chatgptAccountId"`
	ChatGPTPlanType  *string `json:"chatgptPlanType"`
}

type AttestationGenerateParams struct{}
type AttestationGenerateResponse struct {
	Token string `json:"token"`
}

type CurrentTimeReadParams struct {
	ThreadID string `json:"threadId"`
}

type CurrentTimeReadResponse struct {
	CurrentTimeAt int64 `json:"currentTimeAt"`
}

type LegacyApplyPatchApprovalParams struct {
	ConversationID string                     `json:"conversationId"`
	CallID         string                     `json:"callId"`
	FileChanges    map[string]json.RawMessage `json:"fileChanges"`
	Reason         *string                    `json:"reason"`
	GrantRoot      *string                    `json:"grantRoot"`
}

type LegacyApprovalResponse struct {
	Decision any `json:"decision"`
}

// Legacy approval requests use ReviewDecision, whose denial spellings differ
// from the v2 approval methods above.
const (
	LegacyReviewDecisionApproved           = "approved"
	LegacyReviewDecisionApprovedForSession = "approved_for_session"
	LegacyReviewDecisionDenied             = "denied"
	LegacyReviewDecisionTimedOut           = "timed_out"
	LegacyReviewDecisionAbort              = "abort"
)

type LegacyExecCommandApprovalParams struct {
	ConversationID string            `json:"conversationId"`
	CallID         string            `json:"callId"`
	ApprovalID     *string           `json:"approvalId"`
	Command        []string          `json:"command"`
	CWD            string            `json:"cwd"`
	Reason         *string           `json:"reason"`
	ParsedCommand  []json.RawMessage `json:"parsedCmd"`
}
