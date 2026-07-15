# intercom broker protocol

## NAME

intercom broker protocol — local peer registration, discovery, and message routing

## CONTENTS

- [NAME](#name)
- [SYNOPSIS](#synopsis)
- [DESCRIPTION](#description)
- [TRANSPORT](#transport)
  - [Frame format](#frame-format)
  - [JSON decoding](#json-decoding)
  - [End of stream](#end-of-stream)
  - [Common inbound-frame conditions](#common-inbound-frame-conditions)
- [DATA TYPES](#data-types)
  - [Peer name](#peer-name)
  - [Request ID](#request-id)
  - [Message](#message)
  - [Timestamp](#timestamp)
- [CONNECTION LIFECYCLE](#connection-lifecycle)
  - [Handshake](#handshake)
  - [Registered state](#registered-state)
  - [Shutdown](#shutdown)
- [FRAME REFERENCE](#frame-reference)
  - [`hello`](#hello)
  - [`welcome`](#welcome)
  - [`send`](#send)
  - [`send_ack`](#send_ack)
  - [`list_peers`](#list_peers)
  - [`list_peers_reply`](#list_peers_reply)
  - [`deliver`](#deliver)
  - [`goodbye`](#goodbye)
  - [`error`](#error)
- [ERROR CODES](#error-codes)
- [CORRELATION AND ORDERING](#correlation-and-ordering)
- [DELIVERY SEMANTICS](#delivery-semantics)
- [CLIENT BEHAVIOR](#client-behavior)
- [CLAUDE MCP MAPPING](#claude-mcp-mapping)
  - [Transport profile](#transport-profile)
  - [Request IDs and dispatch](#request-ids-and-dispatch)
  - [`tools/call` result envelopes](#toolscall-result-envelopes)
  - [MCP errors](#mcp-errors)
  - [`send_message`](#send_message)
  - [`list_peers` tool](#list_peers-tool)
  - [Inbound channel notification](#inbound-channel-notification)
- [LIMITS AND DEADLINES](#limits-and-deadlines)
- [SECURITY](#security)
- [VERIFICATION](#verification)
- [NOTES](#notes)
- [SEE ALSO](#see-also)

## SYNOPSIS

    adapter -> broker    hello
    broker  -> adapter   welcome | error | goodbye

    adapter -> broker    send
    broker  -> recipient deliver
    broker  -> sender    send_ack

    adapter -> broker    list_peers
    broker  -> adapter   list_peers_reply

The broker listens on a Unix domain socket. Each connection carries a sequence
of length-prefixed JSON frames. The first client frame is a hello frame.

## DESCRIPTION

The broker maintains an in-memory map from peer names to active Unix socket
connections. A peer registers one name per connection. The broker routes a
send frame to one registered destination and returns a correlated send_ack
frame to the sender. It returns a sorted snapshot of registered peers in
response to list_peers.

The protocol provides no persistent mailbox, replay log, redelivery, or
application-level receipt. State exists only in the broker process.

The Claude adapter maps broker delivery frames to MCP channel notifications.
It maps the send_message and list_peers agent tools to broker requests. The
canonical agent-tool argument, result, and error contract is defined in the
[Command reference](REFERENCE.md#agent-tools).

## TRANSPORT

### Frame format

    +----------------------+-----------------------------+
    | payload length       | JSON payload                |
    | 4 bytes, big-endian  | payload length bytes        |
    +----------------------+-----------------------------+

| Part | Type | Mode | Units | Semantics |
| --- | --- | --- | --- | --- |
| payload length | unsigned 32-bit integer | required | bytes | Gives the JSON payload length. The four-byte prefix is not included. |
| JSON payload | JSON object | required | bytes | Contains a string kind discriminator and the fields for that frame kind. |

The maximum JSON payload length is 262,144 bytes. A payload of exactly 262,144
bytes is permitted. The four-byte prefix is outside this limit.

An outbound frame larger than the limit is rejected before its prefix is
written. The connection remains synchronized. An inbound prefix announcing a
larger payload makes the connection unusable because the unread payload
remains in the stream.

Writes on one connection are serialized. Frame bytes from concurrent writers
do not interleave. Reads are single-consumer operations.

### JSON decoding

The decoder performs these operations in order:

1. It decodes the payload sufficiently to read kind.
2. It rejects a missing, non-string, or unknown kind.
3. It decodes the payload into the structure selected by kind.

Unknown JSON object members are ignored. Missing structure members receive
their JSON zero value. The broker applies the semantic checks stated in
[CONNECTION LIFECYCLE](#connection-lifecycle) and [ERROR CODES](#error-codes);
it does not apply a general required-field schema to raw broker frames.

Malformed JSON, a missing kind, an unknown kind, and a field whose JSON type
cannot decode into the selected structure are bad_frame conditions.

### End of stream

A clean end of stream before any byte of a frame returns EOF. An end of stream
after part of a prefix or payload is a short read. The broker closes the peer
connection in both cases. A clean EOF produces no error frame. A short read
produces a best-effort bad_frame response when the connection still accepts
writes and broker shutdown has not started.

### Common inbound-frame conditions

The following conditions apply whenever the broker reads a client frame. A
frame entry's Errors section lists its additional semantic conditions.

| Condition | Error carrier and broker result |
| --- | --- |
| The four-byte prefix announces more than 262,144 payload bytes. | A best-effort uncorrelated `error` frame carries `oversize`; the broker closes the connection without reading the announced payload. |
| The payload is malformed JSON, omits `kind`, has an unknown `kind`, or contains a field whose JSON type cannot decode for that kind. | A best-effort uncorrelated `error` frame carries `bad_frame`; the broker closes the connection. |
| The stream ends after part of a prefix or payload. | A best-effort uncorrelated `error` frame carries `bad_frame`; the broker closes the connection. |
| The stream ends before any byte of the next frame. | The broker closes the connection without an error frame. |
| The broker does not finish reading the first frame before the configured hello deadline. | An uncorrelated `error` frame carries `hello_timeout` under a one-second raw-socket write deadline; the broker then closes the connection. |
| The first complete decodable frame has a recognized `kind` other than `hello`. | A best-effort uncorrelated `error` frame carries `bad_hello`; the broker closes the connection. |
| A recognized frame other than `send` or `list_peers` follows successful registration. | A best-effort uncorrelated `error` frame carries `bad_frame`; the broker closes the connection. |

The broker can omit a best-effort error when the connection no longer accepts
writes or shutdown has started. [ERROR CODES](#error-codes) defines the exact
diagnostic text and connection action.

## DATA TYPES

### Peer name

| Property | Value |
| --- | --- |
| Type | string |
| Length | 1 through 64 bytes |
| Alphabet | ASCII letters, ASCII digits, hyphen, underscore |
| Regular expression | ^[A-Za-z0-9_-]+$ |
| Comparison | exact and case-sensitive |

The broker validates the name field of hello. It does not apply peer-name
validation to the to field of a raw send frame. Agent-tool adapters validate
to before constructing a send frame.

### Request ID

| Property | Value |
| --- | --- |
| Type | string |
| Interpretation by broker | opaque |
| Generated form used by adapters | 16 lowercase hexadecimal characters |
| Generated entropy | 8 bytes from the operating system cryptographic random source |

The broker copies a request ID without modification. It does not require an ID
to be present, nonempty, or unique. Adapter clients use IDs to correlate
concurrent requests. Failure of the cryptographic random source terminates ID
generation by panic.

### Message

The raw wire type is a JSON string. The broker does not impose a separate
message-length or nonempty check. The encoded send and deliver frames remain
subject to the 262,144-byte frame limit.

The canonical [agent-tool contract](REFERENCE.md#send_message) requires a
nonempty decoded message of at most 204,800 bytes. It also rejects a message
whose JSON escaping would make a worst-case delivery envelope exceed the wire
limit. The worst-case envelope uses a 64-byte sender name, a 16-character
request ID, and an RFC 3339 timestamp.

### Timestamp

The broker constructs timestamps in UTC with whole-second RFC 3339 format:

    2026-07-13T19:42:00Z

The broker creates the timestamp while routing the send request. The timestamp
does not certify model receipt or processing.

## CONNECTION LIFECYCLE

### Handshake

1. The client opens the configured Unix domain socket.
2. The client sends hello as the first frame.
3. The broker reads the complete first frame within the hello deadline.
4. The broker validates the hello frame kind and peer name.
5. The broker rejects a name already present in its registry.
6. The broker registers the connection and sends welcome.
7. The broker clears the read deadline and accepts send and list_peers frames.

The default broker hello deadline is 5 seconds. The standard client applies a
5-second total budget to the hello write and a separate 5-second deadline to
the welcome read.

A connection does not become discoverable until registration succeeds. A
connection is removed from discovery when its handler exits or when delivery
failure causes the broker to drop it.

### Registered state

Only send and list_peers are valid client frames after welcome. Any other
recognized frame kind produces bad_frame and closes the connection.

The broker applies no read deadline to registered requests. It may wait
indefinitely for the next frame.

### Shutdown

Broker shutdown prevents new registration and routing. Each registered peer
receives a best-effort goodbye frame with reason set to shutdown. The goodbye
write has a one-second total budget, including time waiting behind another
write to the same connection. The broker closes the connection after the
write attempt.

Accepted connections that have not registered are closed without goodbye.
A registration that races with shutdown either enters the shutdown peer
snapshot or receives goodbye with reason shutdown and closes.

The default broker idle interval is 10 minutes with no registered peers. Idle
expiration enters the same shutdown sequence. An idle interval is continuous:
registration cancels the active interval, and removal of the last peer starts
a new interval.

## FRAME REFERENCE

Every JSON example in this section is the payload. The four-byte big-endian
payload length precedes it on the socket.

String-field lengths use UTF-8 bytes. A field without a narrower limit in its
entry or referenced data type is bounded by the 262,144-byte complete-payload
limit. Client-to-broker frames are also subject to
[Common inbound-frame conditions](#common-inbound-frame-conditions).

### hello

#### Signature

    {"kind":"hello","name":string,"version":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `hello` | none | Selects the frame type. |
| name | string | required by the protocol | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | empty string | Selects the peer name to register. |
| version | string | required by the standard client | UTF-8 bytes; complete-frame limit | empty string | Reports the adapter version for logging. The broker does not validate or negotiate it. |

#### Semantics

hello is the first frame on a new connection. A valid and available name
registers the connection. The broker then returns welcome.

#### Errors

| Error | Exact condition |
| --- | --- |
| bad_name | name is empty, exceeds 64 bytes, or contains a byte outside the peer-name alphabet. |
| name_taken | Another registered connection has the same case-sensitive name. |
| bad_frame | The JSON or typed fields cannot be decoded. |
| hello_timeout | The broker does not finish reading the first frame before the configured hello deadline. |

An omitted name therefore raises bad_name, not bad_hello. An omitted version is
accepted by the broker as an empty informational value.
[Common inbound-frame conditions](#common-inbound-frame-conditions) define
oversize prefixes, truncated frames, and end-of-stream results.

#### Example

Payload:

    {"kind":"hello","name":"reviewer","version":"example"}

#### See also

[welcome](#welcome), [error](#error), [Peer name](#peer-name).

### welcome

#### Signature

    {"kind":"welcome"}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `welcome` | none | Selects the frame type. |

#### Semantics

welcome confirms that the broker registered the peer name. The client may
issue requests after receiving this frame.

#### Errors

None.

#### Example

Payload:

    {"kind":"welcome"}

#### See also

[hello](#hello), [Connection lifecycle](#connection-lifecycle).

### send

#### Signature

    {"kind":"send","id":string,"to":string,"message":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `send` | none | Selects the frame type. |
| id | string | required by adapter clients | opaque UTF-8 bytes; complete-frame limit | empty string | Correlates deliver and send_ack with this request. |
| to | string | required by adapter clients | UTF-8 bytes; complete-frame limit | empty string | Names the destination peer. Raw broker input is not subject to the peer-name grammar. |
| message | string | required by adapter clients | UTF-8 bytes; complete-frame limit | empty string | Contains the delivered text. |

#### Semantics

The broker looks up to in the current registry. It writes one deliver frame to
the selected connection and then writes send_ack to the sender. The default
delivery-write budget is 5 seconds and includes queue wait behind another
write to the destination.

The standard agent-tool adapter requires valid nonempty to and message values.
The raw broker accepts zero-value fields and applies only routing and frame-size
rules.

#### Errors

| Error | Exact condition |
| --- | --- |
| no_self_send | to exactly equals the sender's registered name. |
| no_such_peer | No registered peer has the value of to at lookup time. This includes an omitted or empty to. |
| oversize | The broker can decode send, but the constructed deliver payload exceeds 262,144 bytes. |
| deliver_failed | The destination exists at lookup time, but its deliver write fails for a reason other than oversize. |

These errors appear in send_ack, not in a standalone error frame.
[Common inbound-frame conditions](#common-inbound-frame-conditions) define
malformed, oversized, truncated, lifecycle-invalid, and end-of-stream results.

#### Example

Payload:

    {"kind":"send","id":"0123456789abcdef","to":"reviewer","message":"Check the test failure."}

#### See also

[deliver](#deliver), [send_ack](#send_ack), [Delivery semantics](#delivery-semantics).

### send_ack

#### Signature

Success:

    {"kind":"send_ack","id":string,"ok":true}

Failure:

    {"kind":"send_ack","id":string,"ok":false,"code":string,"message":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `send_ack` | none | Selects the frame type. |
| id | string | required | opaque UTF-8 bytes; complete-frame limit | empty string | Copies `send.id`. |
| ok | boolean | required | none | false | Reports whether the broker completed the destination socket write. |
| code | string | required when `ok` is false; omitted when `ok` is true | one code from [ERROR CODES](#error-codes) | empty string | Gives the rejection code. |
| message | string | required when `ok` is false; omitted when `ok` is true | UTF-8 bytes; complete-frame limit | empty string | Gives a diagnostic message. |

#### Semantics

ok true means that the broker completed the framed deliver write to the
destination socket. It does not mean that the adapter read the frame, that a
model observed it, that a process acted on it, or that a reply follows.

#### Errors

Failure codes are no_self_send, no_such_peer, oversize, and deliver_failed.
Their conditions appear in [ERROR CODES](#error-codes).

#### Example

Payload:

    {"kind":"send_ack","id":"0123456789abcdef","ok":true}

#### See also

[send](#send), [ERROR CODES](#error-codes), [Delivery semantics](#delivery-semantics).

### list_peers

#### Signature

    {"kind":"list_peers","id":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `list_peers` | none | Selects the frame type. |
| id | string | required by adapter clients | opaque UTF-8 bytes; complete-frame limit | empty string | Correlates list_peers_reply with this request. |

#### Semantics

The broker takes a registry snapshot, excludes the requesting peer, sorts the
remaining case-sensitive names lexicographically, and returns
list_peers_reply.

#### Errors

No application-level error is defined. Connection, framing, and write failures
can prevent a reply.
[Common inbound-frame conditions](#common-inbound-frame-conditions) define
malformed, oversized, truncated, lifecycle-invalid, and end-of-stream results.

#### Example

Payload:

    {"kind":"list_peers","id":"fedcba9876543210"}

#### See also

[list_peers_reply](#list_peers_reply), [Peer name](#peer-name).

### list_peers_reply

#### Signature

    {"kind":"list_peers_reply","id":string,"peers":[string,...]}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `list_peers_reply` | none | Selects the frame type. |
| id | string | required | opaque UTF-8 bytes; complete-frame limit | empty string | Copies `list_peers.id`. |
| peers | array of strings | required | zero or more 1–64-byte peer names; complete-frame limit | null | Lists other registered peer names in lexicographic order. The broker emits an empty array when none exist. |

#### Semantics

The result reflects one registry snapshot. A listed peer may disconnect before
the next send. The requester never appears in peers.

#### Errors

None.

#### Example

Payload:

    {"kind":"list_peers_reply","id":"fedcba9876543210","peers":["builder","reviewer"]}

#### See also

[list_peers](#list_peers), [no_such_peer](#error-codes).

### deliver

#### Signature

    {"kind":"deliver","id":string,"from":string,"message":string,"timestamp":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `deliver` | none | Selects the frame type. |
| id | string | conditional | opaque UTF-8 bytes; complete-frame limit | omitted | Copies `send.id`. The encoder omits this member when the ID is empty. |
| from | string | required | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | empty string | Contains the sender's registered and validated peer name. |
| message | string | required | UTF-8 bytes; complete-frame limit | empty string | Copies `send.message`. |
| timestamp | string | required | whole-second UTC RFC 3339 timestamp | empty string | Gives the broker routing time. |

#### Semantics

The broker emits deliver only after selecting a registered destination. The
standard client treats it as an unsolicited inbound event and does not use id
to acknowledge it.

#### Errors

The destination sends no broker-protocol response to deliver. A failed or
timed-out broker write produces deliver_failed for the original sender and
causes the broker to remove and close the destination. An oversize constructed
frame produces oversize for the original sender without dropping either
connection.

#### Example

Payload:

    {"kind":"deliver","id":"0123456789abcdef","from":"builder","message":"Check the test failure.","timestamp":"2026-07-13T19:42:00Z"}

#### See also

[send](#send), [send_ack](#send_ack), [Claude MCP mapping](#claude-mcp-mapping).

### goodbye

#### Signature

    {"kind":"goodbye","reason":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `goodbye` | none | Selects the frame type. |
| reason | string | required | exact literal `shutdown` from the standard broker | empty string | Describes why the broker closes the connection. |

#### Semantics

The broker emits goodbye as a terminal, best-effort frame during shutdown. The
broker-generated reason is shutdown. The standard client reports a
disconnected lifecycle event, invokes its goodbye callback, and closes the
connection state after receiving the frame.

#### Errors

None. The write can fail or time out, in which case the peer observes only
connection closure.

#### Example

Payload:

    {"kind":"goodbye","reason":"shutdown"}

#### See also

[Shutdown](#shutdown), [Client behavior](#client-behavior).

### error

#### Signature

    {"kind":"error","id":string,"code":string,"message":string}

#### Fields

| Field | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| kind | string | required | exact literal `error` | none | Selects the frame type. |
| id | string | conditional | opaque UTF-8 bytes; complete-frame limit | omitted | Correlates an error with a request when the broker has a usable request ID. |
| code | string | required | one code from [ERROR CODES](#error-codes) | empty string | Gives the protocol error code. |
| message | string | required | UTF-8 bytes; complete-frame limit | empty string | Gives the diagnostic message. |

#### Semantics

Handshake and framing failures use error. An uncorrelated error omits id.
The standard broker omits id from every error it emits. The frame type permits
a nonempty id, and the standard client routes such a frame to the matching
pending request. It logs an error with empty id as unsolicited.

The broker sends routing rejections in send_ack rather than error.

#### Errors

The complete code set is defined in [ERROR CODES](#error-codes).

#### Example

Payload:

    {"kind":"error","code":"bad_name","message":"invalid peer name"}

#### See also

[ERROR CODES](#error-codes), [hello](#hello), [send_ack](#send_ack).

## ERROR CODES

| Code | Carrier | Exact condition | Broker action | Diagnostic message |
| --- | --- | --- | --- | --- |
| bad_hello | error | The first complete and decodable frame has a recognized kind other than hello. | Closes the connection. | first frame must be hello |
| bad_name | error | hello.name is empty, longer than 64 bytes, or contains a byte outside ASCII letters, digits, hyphen, and underscore. | Closes the connection. | invalid peer name |
| name_taken | error | A registered connection already owns the exact hello.name value. | Closes the new connection and leaves the existing peer registered. | peer "NAME" already connected |
| hello_timeout | error | The broker does not finish reading the first frame before the configured hello deadline. | Attempts the error with a one-second raw socket write deadline, then closes the connection. | hello not received within deadline |
| no_such_peer | send_ack | send.to does not name a registered peer at lookup time. | Preserves the sender connection. | no such peer: NAME |
| no_self_send | send_ack | send.to exactly equals the sender's registered name. | Preserves the sender connection. | cannot send to self |
| deliver_failed | send_ack | The destination is registered at lookup, but the deliver write returns a non-oversize error, including a delivery deadline expiration or connection failure. | Removes and closes the destination; preserves the sender when its acknowledgement can be written. | delivery failed: DETAIL |
| oversize | error | An inbound length prefix announces a payload larger than 262,144 bytes. | Attempts an uncorrelated error and closes the source connection. | frame exceeds max size |
| oversize | send_ack | A decoded send fits the wire limit, but the broker-generated deliver payload exceeds it. | Rejects only the request and preserves both connections. | message exceeds delivery frame size |
| bad_frame | error | A frame is malformed JSON, has missing or unknown kind, has an incompatible typed field, ends in a short read, or has a post-handshake kind other than send or list_peers. | Attempts an error and closes the connection. During shutdown, a read failure can close without an error. | Decoder detail, or unexpected frame KIND after hello |

All error writes are best effort. A broken connection may close before the
client receives the error.

hello_timeout applies to a blocked or incomplete first-frame read. A clean EOF
before a first-frame byte closes without an error. bad_hello applies only after
a complete recognized non-hello frame has decoded.

## CORRELATION AND ORDERING

The standard adapter creates a fresh request ID for each send and list_peers
call. A pending-call map associates the ID with one response channel.

The broker copies IDs as follows:

| Source | Destination |
| --- | --- |
| send.id | deliver.id |
| send.id | send_ack.id |
| list_peers.id | list_peers_reply.id |

The broker handles each connection in a separate goroutine. Requests on one
source connection are read sequentially, but delivery writes to a destination
may queue behind writes initiated by other source connections. The per-socket
write gate preserves complete frame boundaries.

The protocol does not require globally ordered messages across different
source connections. Correlation uses IDs rather than response position.

## DELIVERY SEMANTICS

A successful send_ack certifies only that the broker completed the deliver
frame write to the destination Unix socket. The following events are outside
that acknowledgement:

- the destination adapter reads the frame;
- the destination adapter writes an MCP notification;
- the model provider receives the notification;
- the model observes or acts on the message;
- the destination sends a reply.

The broker stores no message after routing. It does not retry a failed write.
A process failure after send_ack can therefore lose the message. Peer discovery
is also transient and contains only connections registered at snapshot time.

Self-send is rejected. The protocol does not detect or prevent reply loops
between two or more peers.

## CLIENT BEHAVIOR

The standard broker client owns at most one broker connection. Connect is
idempotent after a successful handshake. Concurrent Connect calls serialize so
that one peer does not race two hello frames under the same name.

When the initial socket dial fails with connection refused or path-not-found,
the client starts the configured broker binary and retries. It performs one
initial dial and up to four post-spawn dials. The scheduled waits before those
dials are at most 100 milliseconds, 300 milliseconds, 1 second, and 3 seconds.
Spawned-broker exit ends the current wait early and is followed by an immediate
dial; a nonzero exit ends the sequence when that dial also fails. Each dial has
a one-second timeout. Other initial dial failures, including permission errors,
do not trigger broker start.

The client applies a five-second total budget to each request write. That
budget includes encoding, waiting for the per-connection write gate, and socket
I/O. Waiting for send_ack or list_peers_reply has no independent protocol
timeout; the calling context controls it.

EOF, goodbye, read failure, and non-oversize write failure disconnect the
client and fail all pending requests with a disconnected result. A later Send
or ListPeers call reconnects on demand. An outbound oversize rejection removes
only that pending request because no frame bytes were written.

The connection-event stream retains only the newest unread state. Each
successful handshake increments its generation. The stream reports connected,
disconnected, and closed states; disconnection causes distinguish EOF,
goodbye, read error, and write error.

## CLAUDE MCP MAPPING

### Transport profile

The Claude adapter serves newline-delimited JSON-RPC 2.0 on standard input and
standard output. One complete JSON-RPC object occupies one line. Empty lines
are ignored. The scanner buffer ceiling is 8 MiB; an input token that cannot
fit produces a fatal read error.

The supported request methods are:

| Method | Parameters | Result |
| --- | --- | --- |
| initialize | protocolVersion string; missing or undecodable parameters are tolerated | protocolVersion, capabilities, serverInfo, and instructions |
| tools/list | ignored | Definitions for send_message and list_peers |
| tools/call | name string and arguments object | One text content item and optional isError |
| ping | ignored | Empty object |

The supported inbound notification is notifications/initialized. It marks the
MCP handshake complete and produces no response. Other inbound notifications
are ignored.

initialize echoes a nonempty client protocolVersion. It uses 2025-11-25 when
the client supplies no usable value. The result declares tools and the
experimental claude/channel capability. serverInfo.name is intercom and
serverInfo.version is the running binary version. Tool enumeration order is
unspecified.

### Request IDs and dispatch

Malformed JSON lines are discarded without a response. A line with a non-null
jsonrpc or method value that cannot decode as a JSON string is likewise
discarded during envelope decoding. JSON null decodes as an empty string for
either member.

A request ID is a JSON string or number. The response preserves the lexical
spelling of that string or number instead of decoding and re-encoding it. A
missing or null ID classifies the message as a notification and produces no
response when jsonrpc is exactly 2.0.

Boolean, object, and array IDs are invalid. They produce an Invalid Request
response with id set to null. After envelope decoding, ID validation precedes
jsonrpc validation. An invalid ID therefore produces the ID diagnostic even
when jsonrpc is absent, null, or a string other than 2.0. A valid string,
number, absent, or null ID proceeds to jsonrpc validation; an invalid decoded
jsonrpc value then produces an Invalid Request response, including for a
message otherwise classified as a notification.

tools/call handlers run concurrently with no Intercom concurrency cap. One
goroutine serves each call. Responses can therefore occur in a different
order from requests. Output writes are serialized so JSON-RPC lines do not
interleave.

### tools/call result envelopes

A successful agent-tool call returns exactly one text content item and omits
isError:

    {"jsonrpc":"2.0","id":ID,"result":{"content":[{"type":"text","text":TEXT}]}}

An agent-tool validation or broker-operation failure returns exactly one text
content item and sets isError to true:

    {"jsonrpc":"2.0","id":ID,"result":{"content":[{"type":"text","text":TEXT}],"isError":true}}

ID is the lexically preserved string or number from the request. TEXT is a
JSON string containing the canonical result or diagnostic text defined in the
[agent-tool reference](REFERENCE.md#agent-tools).

A protocol-level failure uses an error member and omits result:

    {"jsonrpc":"2.0","id":ID_OR_NULL,"error":{"code":INTEGER,"message":TEXT}}

ID_OR_NULL is the valid request ID, or null when no valid request ID is
available. The code and message conditions are defined in [MCP errors](#mcp-errors).

### MCP errors

| Code | Name | Exact condition | Message |
| --- | --- | --- | --- |
| -32600 | Invalid Request | id is a boolean, object, or array. The response ID is null, and this check occurs before jsonrpc validation. | `id must be a string, number, or null` |
| -32600 | Invalid Request | id is valid and jsonrpc is absent, null, or a decoded string other than exactly 2.0. The response copies a valid string or number ID and otherwise uses null. A non-null non-string jsonrpc value is discarded during envelope decoding instead. | `expected jsonrpc 2.0` |
| -32601 | Method not found | The request method is not initialize, tools/list, tools/call, or ping. | `method not found: METHOD` |
| -32601 | Method not found | tools/call names an unregistered tool. | `unknown tool: NAME` |
| -32602 | Invalid params | tools/call params cannot decode into the method parameter structure. | `invalid tools/call params: DETAIL` |
| -32603 | Internal error | A tool handler returns a protocol-level error. | `tool "NAME": DETAIL` |
| -32603 | Internal error | A tool handler panics. | `tool "NAME": panic: DETAIL` |

Agent-tool validation and broker-routing failures are not MCP error objects.
They return a tools/call result whose isError member is true and whose content
contains the diagnostic text.

### send_message

#### Signature

    {"jsonrpc":"2.0","id":ID,"method":"tools/call","params":{"name":"send_message","arguments":{"to":string,"message":string}}}

#### Arguments

params.name is send_message. params.arguments is passed to the provider-neutral
tool decoder. The canonical member types, modes, limits, validation rules,
result text, and error conditions are defined in the
[send_message reference](REFERENCE.md#send_message).

At the shared decoder boundary, empty argument bytes and a non-object JSON
value return exactly `send_message arguments must be an object`. Object-shaped
malformed JSON, an unknown object member, and a trailing JSON value return
`decode args: DETAIL`. MCP defaults an omitted params.arguments member to `{}`
before this validation, so omission produces the canonical missing-to error.
A syntactically malformed JSON-RPC line is discarded before tool dispatch.

#### Semantics

The adapter connects or reconnects to the broker, sends one send frame, waits
for the matching response, and maps the provider-neutral tool result to the
[tools/call result envelope](#toolscall-result-envelopes).

#### Errors

Provider-neutral validation, broker-operation, and routing failures produce a
tools/call result with isError true. The exhaustive conditions and exact text
are defined in the [send_message reference](REFERENCE.md#send_message).

#### Example

    {"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"to":"reviewer","message":"Check the test failure."}}}

#### See also

[send](#send), [send_ack](#send_ack), [Delivery semantics](#delivery-semantics),
[agent-tool reference](REFERENCE.md#agent-tools).

### list_peers tool

#### Signature

    {"jsonrpc":"2.0","id":ID,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}

#### Arguments

params.name is list_peers. MCP defaults an omitted params.arguments member to
an empty object before provider-neutral tool validation. The canonical
argument and error contract is defined in the
[list_peers reference](REFERENCE.md#list_peers).

#### Semantics

The adapter connects or reconnects to the broker, sends list_peers, waits for
the matching reply, and maps the provider-neutral tool result to the
[tools/call result envelope](#toolscall-result-envelopes).

#### Errors

Provider-neutral validation and broker-operation failures produce a tools/call
result with isError true. The exhaustive conditions and exact text are defined
in the [list_peers reference](REFERENCE.md#list_peers).

#### Example

    {"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_peers","arguments":{}}}

#### See also

[list_peers](#list_peers), [list_peers_reply](#list_peers_reply),
[agent-tool reference](REFERENCE.md#agent-tools).

### Inbound channel notification

#### Signature

    {"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":string,"meta":{"from":string,"timestamp":string}}}

#### Arguments

| Member | Type | Mode | Units or limits | Default when omitted | Semantics |
| --- | --- | --- | --- | --- | --- |
| params.content | string | required | UTF-8 bytes; source broker-frame limit | empty string | Copies `deliver.message`. |
| params.meta.from | string | required | 1–64 ASCII bytes; `[A-Za-z0-9_-]` | empty string | Copies `deliver.from`. |
| params.meta.timestamp | string | required | whole-second UTC RFC 3339 timestamp | empty string | Copies `deliver.timestamp`. |

#### Semantics

The Claude adapter emits the notification from the broker client's delivery
callback. deliver.id is not exposed in the channel notification. A notification
write error is logged. No broker acknowledgement flows from Claude Code to the
sender.

The adapter normally connects eagerly after notifications/initialized. An
eager connection failure is nonfatal and is retried by a later tool call. A
tool call can establish the broker connection before MCP initialization, so
the server does not guarantee that every outbound channel notification follows
notifications/initialized.

#### Errors

The notification has no response. Standard-output failure prevents delivery
to Claude Code and is logged by the adapter.

#### Example

    {"jsonrpc":"2.0","method":"notifications/claude/channel","params":{"content":"Check the test failure.","meta":{"from":"builder","timestamp":"2026-07-13T19:42:00Z"}}}

#### See also

[deliver](#deliver), [Delivery semantics](#delivery-semantics).

## LIMITS AND DEADLINES

| Boundary | Default or limit | Units | Condition |
| --- | --- | --- | --- |
| Broker JSON payload | 262,144 | bytes | Excludes the four-byte prefix. |
| Peer name | 64 | bytes | ASCII alphabet only. |
| Agent-tool message | 204,800 | decoded bytes | Also subject to encoded-delivery sizing. |
| Claude MCP input scanner | 8,388,608 | bytes of buffer capacity | An input token that cannot fit is fatal. |
| Accepted broker connections | no application cap | connections | Operating-system socket, descriptor, and memory limits apply. |
| Registered broker peers | no application cap | peers | Unique peer names and operating-system resource limits apply. |
| Claude tools/call handlers | no Intercom cap | concurrent handlers | One goroutine serves each call; process and operating-system resource limits apply. |
| Broker first-frame read | 5 | seconds | Ends after a complete first frame. |
| Standard-client hello write | 5 | seconds | Includes encode, queue, and write. |
| Standard-client welcome read | 5 | seconds | Separate from hello-write budget. |
| Standard-client request write | 5 | seconds | Applies to send and list_peers. |
| Broker deliver write | 5 | seconds | Includes destination write-queue wait. |
| Broker shutdown goodbye write | 1 | second | Includes connection write-queue wait. |
| Broker welcome, send_ack, list_peers_reply, and ordinary error writes | none | — | These writes can wait behind the connection write gate or socket until connection failure or broker shutdown unblocks them. hello_timeout error writes are the bounded exception. |
| Registered broker read | none | — | Waits until data, closure, or shutdown. |
| Request reply wait | caller context | — | Has no independent broker-client timeout. |
| Broker idle exit | 10 | minutes | Continuous period with zero registered peers. |
| Initial and retry socket dial | 1 | second per dial | One initial dial and up to four post-spawn dials. |
| Post-spawn retry waits | 100 ms, 300 ms, 1 s, 3 s | maximum time per iteration | Each wait ends early when spawned-broker exit is observed; the following dial still occurs. |

Broker library options can replace the hello, delivery, and idle defaults.
A nonpositive wire write timeout disables the wire-level deadline. The command
interface documents which broker options are user-configurable.

## SECURITY

The transport is local to one Unix host. The broker changes the socket mode to
0600 and creates its singleton lock file with mode 0600. Filesystem access to
the socket is the admission boundary.

The protocol performs no operating-system credential exchange and no
application authentication. A process able to connect to the socket can claim
any available valid peer name, enumerate peers, and send messages to them.

Inbound peer messages become agent input at the adapter boundary. Peer names
identify connections; they do not establish trust. Message content can leave
the host when the connected agent provider processes it.

## VERIFICATION

The package-test and broker-framing commands in this section run from the
repository root and require the Go version declared by `go.mod`. The installed-
binary MCP ping is independent of the repository working directory.

The protocol and adapter examples are exercised by the package tests:

    go test ./internal/wire ./internal/broker ./internal/brokerclient \
      ./internal/intercomtools ./internal/mcp ./internal/shim

The [broker framing example](examples/broker-client.go) starts an isolated
broker, writes length-prefixed hello and list_peers frames without the standard
broker client, and verifies both responses:

    go run ./docs/examples/broker-client.go

Expected standard output:

    welcome
    peers 0

The MCP ping path is runnable against an installed binary:

    printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"ping"}' |
      INTERCOM_NAME=protocol-check intercom shim

Expected standard output:

    {"jsonrpc":"2.0","id":1,"result":{}}

The shim writes diagnostics only to standard error. End of standard input
terminates this example without starting a broker because no initialized
notification or tool call occurs.

## NOTES

The socket acknowledgement boundary and the model-observation boundary are
different. Applications requiring durable delivery, offline queues, duplicate
suppression, or model-level acknowledgements require a protocol above this
one.

The raw broker decoder accepts unknown JSON members and zero values except
where the broker applies an explicit semantic check. Agent adapters expose the
stricter schemas defined in the [agent-tool reference](REFERENCE.md#agent-tools).

The protocol performs no version negotiation. Unknown-field tolerance and an
optional delivery ID do not establish compatibility between arbitrary
Intercom builds. One broker socket should contain clients from the same build.

## SEE ALSO

[Command reference](REFERENCE.md), [Handbook](HANDBOOK.md),
[Architecture](ARCHITECTURE.md), [Project synopsis](../README.md).
