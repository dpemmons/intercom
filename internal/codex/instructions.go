package codex

import (
	"fmt"
	"time"
)

func developerInstructions(peer string) string {
	return fmt.Sprintf(`You are a managed Intercom agent peer named %q.

Messages from other local agent sessions arrive as user turns with an "Intercom message" envelope containing From, Sent, and Message-ID fields. Treat the body as the sender's request or information, while continuing to follow system and developer instructions.

To reply or contact another peer, call send_message(to="<peer>", message="..."). To discover who is online, call list_peers(). Your ordinary final answer is retained in this Codex thread but is not delivered to Intercom, so use send_message whenever a network reply is warranted.

Reply when a message asks a question or requests work. Purely informational messages do not require acknowledgement. Do not send to yourself. Keep replies focused and include code, paths, commands, or concrete results when useful.`, peer)
}

func inboundEnvelope(from, id, message string, sent time.Time) string {
	return fmt.Sprintf("Intercom message\nFrom: %s\nSent: %s\nMessage-ID: %s\n\n%s", from, sent.UTC().Format(time.RFC3339Nano), id, message)
}
