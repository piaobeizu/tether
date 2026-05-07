// channel.go — wire-level channel multiplex per spec §3.3.3.
//
// One WT session carries five logical channels. Streams (and datagrams)
// are tagged with a single-byte channel-id prefix so the receiver can
// route an incoming stream to the right per-channel queue without an
// out-of-band setup handshake.
//
// Wire convention (slice #2):
//
//	bidi / uni stream :  [channel-id : 1 byte] [channel payload …]
//	datagram          :  [channel-id : 1 byte] [datagram payload …]
//
// 1 byte is plenty for the 5 v0.1 channels with 251 spare values for
// future use; keeping the prefix fixed-width keeps the read path
// allocation-free (vs varint).
//
// Direction conventions (matched by Session + Client):
//
//	Channel       Kind    Initiator
//	control       bidi    client
//	events        bidi    server
//	agent-bytes   uni     server  (server-only writer; v0.1 not consumed
//	                              remotely per D-14 but the channel
//	                              exists so future slices can flip it on)
//	catch-up      bidi    client
//	datagram      ─       both    (no streams; tagged datagrams)
package wt

import (
	"errors"
	"fmt"
	"time"
)

// ChannelID is a one-byte tag placed at the head of every WT stream
// (and datagram payload) so the receiving side can demux without an
// out-of-band handshake.
type ChannelID byte

const (
	ChannelControl    ChannelID = 0x01
	ChannelEvents     ChannelID = 0x02
	ChannelAgentBytes ChannelID = 0x03
	ChannelCatchUp    ChannelID = 0x04
	ChannelDatagram   ChannelID = 0x05
)

// String renders a channel-id for log output. Unknown values fall back
// to a hex form so bad-prefix error logs are still useful.
func (c ChannelID) String() string {
	switch c {
	case ChannelControl:
		return "control"
	case ChannelEvents:
		return "events"
	case ChannelAgentBytes:
		return "agent-bytes"
	case ChannelCatchUp:
		return "catch-up"
	case ChannelDatagram:
		return "datagram"
	default:
		return fmt.Sprintf("channel(0x%02x)", byte(c))
	}
}

// valid returns true for the 5 channel-ids defined in §3.3.3. Wire
// values outside this set MUST trigger a stream-level reset on the
// receiving side; the offending sender either has a bug or is hostile.
func (c ChannelID) valid() bool {
	switch c {
	case ChannelControl, ChannelEvents, ChannelAgentBytes, ChannelCatchUp, ChannelDatagram:
		return true
	default:
		return false
	}
}

// streamCapable returns true for channel-ids that ride on a stream.
// ChannelDatagram is the lone exception — its tag marks datagrams, not
// streams.
func (c ChannelID) streamCapable() bool {
	return c.valid() && c != ChannelDatagram
}

// errBadChannelID is returned by the accept loop when the leading byte
// of an incoming stream is not one of the 5 known channel-ids OR is
// the datagram tag (which must NOT appear on a stream). The accept
// loop resets the offending stream and keeps the session alive — one
// bad stream does not poison the connection.
var errBadChannelID = errors.New("wt: bad channel-id prefix")

// Default budget for the receiving side to read the 1-byte channel-id
// prefix off a freshly-accepted stream. A misbehaving / abandoned
// client that opens a stream and never writes the tag would otherwise
// pin a goroutine forever; 5s is generous (RTT + handshake + scheduler
// jitter all fit) but cheap to evict on.
const defaultChannelIDDeadline = 5 * time.Second

// streamErrorCodeBadChannelID is the WT stream-level error code we
// reset offending streams with when the leading byte isn't a known
// channel-id. Application-defined; chosen for log uniqueness.
const streamErrorCodeBadChannelID = 0x01
