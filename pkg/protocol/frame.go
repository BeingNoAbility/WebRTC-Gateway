package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// ProtocolVersion is the P2PMux V8 wire protocol version.
	ProtocolVersion = 8

	// HeaderSize is the fixed 8-byte wire frame header:
	// [0:4] StreamID (uint32 BE: 0 on p2pctrl, strictly monotonic >= 1 per data lane)
	// [4]   Type     (uint8)
	// [5]   Flags    (uint8)
	// [6:8] Length   (uint16 BE)
	HeaderSize = 8

	// MaxPayloadSize is the maximum payload expressible in uint16 Length.
	MaxPayloadSize = 65535

	// DefaultChunkSize aligns a single wire frame cleanly to 32 KiB (32760 + 8 = 32768).
	DefaultChunkSize = 32760

	// MinNegotiatedMaxMsgSize is the strict minimum SCTP maxMessageSize floor (4096B).
	MinNegotiatedMaxMsgSize = 4096

	// MinChunkSize matches MinNegotiatedMaxMsgSize - HeaderSize (4088B).
	MinChunkSize = MinNegotiatedMaxMsgSize - HeaderSize

	// MaxTotalHeadersSize caps an assembled HEADERS block (64 KiB).
	MaxTotalHeadersSize = 64 * 1024

	// MaxHeaderChunks caps total frames in one HEADERS message (supports 64 KiB even at 4 KiB MMS).
	MaxHeaderChunks = 17

	// MaxControlJSONSize caps an assembled control-plane JSON envelope (128 KiB, chunked with FlagMore).
	MaxControlJSONSize = 128 * 1024

	// MaxControlChunks caps total frames in one chunked control message (33 * 4088 >= 128 KiB).
	MaxControlChunks = 34

	// MaxWSMessageBytes is the maximum reassembled WebSocket message size (16 MiB).
	MaxWSMessageBytes = 16 * 1024 * 1024

	// MaxWSMessageChunks caps total sub-frames in one streaming WebSocket message (16384 chunks).
	MaxWSMessageChunks = 16384

	// MaxWSClosePayloadBytes enforces RFC 6455 close frame limit (2-byte code + <=123-byte reason).
	MaxWSClosePayloadBytes = 125

	// Pre-negotiated DataChannel layout:
	//   - id=0 ("p2pctrl"): control-plane ordered reliable DC (StreamID == 0)
	//   - id=1..12 ("p2p_01".."p2p_12"): multiplexed ordered reliable data lanes carrying both
	//     HTTP and WebSocket streams by strictly monotonic StreamID >= 1 (zero slot reuse, zero DCEP WS).
	ControlChannelID    uint16 = 0
	ControlChannelLabel        = "p2pctrl"
	DefaultPoolSize            = 12
	MaxPoolSize                = 16
	DefaultMaxWSConns          = 64
)

// Control-plane frame types (exclusively allowed on p2pctrl, id=0, StreamID == 0).
const (
	TypeSettings      uint8 = 0x01 // JSON (chunkable via FlagMore)
	TypeAuthChallenge uint8 = 0x02 // JSON (chunkable via FlagMore)
	TypeAuthResponse  uint8 = 0x03 // JSON (chunkable via FlagMore)
	TypeAuthResult    uint8 = 0x04 // JSON (chunkable via FlagMore)
	TypeCapReq        uint8 = 0x05 // JSON (chunkable via FlagMore)
	TypeCapResp       uint8 = 0x06 // JSON (chunkable via FlagMore)
	TypePing          uint8 = 0x08 // strictly 8-byte payload, Flags == 0
	TypePong          uint8 = 0x09 // strictly 8-byte payload, Flags == 0
	TypeGoAway        uint8 = 0x0A // strictly 4-byte ErrorCode, Flags == 0
)

// Stream data-lane frame types (in-band on p2p_01..N, StreamID >= 1).
const (
	TypeHeaders  uint8 = 0x10 // Flags: FlagFin | FlagMore (multi-frame headers reuse TypeHeaders)
	TypeData     uint8 = 0x12 // Flags: FlagFin (HTTP body; 0B without FlagFin forbidden)
	TypeCredit   uint8 = 0x13 // strictly 4-byte pure payload byte delta in [1, 0x7fffffff], Flags == 0
	TypeReset    uint8 = 0x14 // strictly 4-byte ErrorCode, Flags == 0
	TypeWSText   uint8 = 0x21 // Flags: FlagMsgFin (0B without FlagMsgFin forbidden)
	TypeWSBinary uint8 = 0x22 // Flags: FlagMsgFin (0B without FlagMsgFin forbidden)
	TypeWSClose  uint8 = 0x24 // 2..125 bytes: [u16 code][valid utf8 reason], Flags == 0
)

// Frame flags.
const (
	FlagNone   uint8 = 0x00
	FlagFin    uint8 = 0x01 // Half-close HTTP request/response direction
	FlagMsgFin uint8 = 0x02 // Final chunk of a streaming WebSocket message
	FlagMore   uint8 = 0x04 // Additional chunk follows for TypeHeaders or JSON control frames
)

// ErrorCode is carried in RESET (4 bytes) and GOAWAY (4 bytes).
type ErrorCode uint32

const (
	ErrCodeNoError          ErrorCode = 0
	ErrCodeProtocolError    ErrorCode = 1
	ErrCodeInternalError    ErrorCode = 2
	ErrCodeFlowControlError ErrorCode = 3
	ErrCodeRefusedStream    ErrorCode = 4
	ErrCodeCancel           ErrorCode = 5
	ErrCodeFrameSizeError   ErrorCode = 6
	ErrCodeAuthRequired     ErrorCode = 7
	ErrCodeForbidden        ErrorCode = 8
	ErrCodeGoingAway        ErrorCode = 9
)

func (c ErrorCode) String() string {
	switch c {
	case ErrCodeNoError:
		return "NO_ERROR"
	case ErrCodeProtocolError:
		return "PROTOCOL_ERROR"
	case ErrCodeInternalError:
		return "INTERNAL_ERROR"
	case ErrCodeFlowControlError:
		return "FLOW_CONTROL_ERROR"
	case ErrCodeRefusedStream:
		return "REFUSED_STREAM"
	case ErrCodeCancel:
		return "CANCEL"
	case ErrCodeFrameSizeError:
		return "FRAME_SIZE_ERROR"
	case ErrCodeAuthRequired:
		return "AUTH_REQUIRED"
	case ErrCodeForbidden:
		return "FORBIDDEN"
	case ErrCodeGoingAway:
		return "GOING_AWAY"
	default:
		return fmt.Sprintf("UNKNOWN_ERROR(%d)", uint32(c))
	}
}

var (
	ErrFrameTooShort       = errors.New("protocol: frame shorter than 8-byte header")
	ErrFrameLengthMismatch = errors.New("protocol: frame byte length does not match header length")
	ErrPayloadTooLarge     = errors.New("protocol: payload exceeds 65535 bytes")
	ErrInvalidFrameType    = errors.New("protocol: unknown frame type")
	ErrInvalidFlags        = errors.New("protocol: invalid flags for frame type")
	ErrInvalidStreamID     = errors.New("protocol: invalid stream ID for frame type")
	ErrInvalidPayloadSize  = errors.New("protocol: invalid payload size for frame type")
	ErrInvalidCreditDelta  = errors.New("protocol: credit delta must be in [1, 0x7fffffff]")
)

// PoolChannelID returns the pre-negotiated SCTP DataChannel ID for lane i (0-based -> id 1..16).
func PoolChannelID(laneIdx int) uint16 {
	if laneIdx < 0 || laneIdx >= 0xFFFF {
		return 0
	}
	return uint16(1 + laneIdx) // #nosec G115 -- bounded to [1, 0xFFFF]
}

// PoolChannelLabel returns the canonical DataChannel label for lane i (0-based -> "p2p_01".."p2p_16").
func PoolChannelLabel(laneIdx int) string {
	return fmt.Sprintf("p2p_%02d", laneIdx+1)
}

// IsControlFrameType reports whether frameType belongs exclusively on p2pctrl (id=0).
func IsControlFrameType(frameType uint8) bool {
	switch frameType {
	case TypeSettings, TypeAuthChallenge, TypeAuthResponse, TypeAuthResult,
		TypeCapReq, TypeCapResp, TypePing, TypePong, TypeGoAway:
		return true
	default:
		return false
	}
}

// IsControlJSONFrameType reports whether frameType is a chunkable JSON control message.
func IsControlJSONFrameType(frameType uint8) bool {
	return frameType >= TypeSettings && frameType <= TypeCapResp
}

// IsStreamFrameType reports whether frameType is valid on a multiplexed data lane (p2p_01..N).
func IsStreamFrameType(frameType uint8) bool {
	switch frameType {
	case TypeHeaders, TypeData, TypeCredit, TypeReset, TypeWSText, TypeWSBinary, TypeWSClose:
		return true
	default:
		return false
	}
}

// Frame is a decoded V8 wire frame.
type Frame struct {
	StreamID uint32
	Type     uint8
	Flags    uint8
	Payload  []byte
}

// ValidateFrameMeta checks structural invariants shared by Encode and Decode.
func ValidateFrameMeta(streamID uint32, frameType, flags uint8, payloadLen int) error {
	if payloadLen < 0 || payloadLen > MaxPayloadSize {
		return ErrPayloadTooLarge
	}
	switch frameType {
	case TypeSettings, TypeAuthChallenge, TypeAuthResponse, TypeAuthResult,
		TypeCapReq, TypeCapResp:
		if streamID != 0 {
			return ErrInvalidStreamID
		}
		if flags & ^FlagMore != 0 {
			return ErrInvalidFlags
		}
		if payloadLen == 0 && flags&FlagMore != 0 {
			return ErrInvalidPayloadSize
		}
	case TypePing, TypePong:
		if streamID != 0 {
			return ErrInvalidStreamID
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 8 {
			return ErrInvalidPayloadSize
		}
	case TypeGoAway:
		if streamID != 0 {
			return ErrInvalidStreamID
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 4 {
			return ErrInvalidPayloadSize
		}
	case TypeCredit, TypeReset:
		if streamID == 0 {
			return ErrInvalidStreamID
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 4 {
			return ErrInvalidPayloadSize
		}
	case TypeHeaders:
		if streamID == 0 {
			return ErrInvalidStreamID
		}
		if flags & ^(FlagFin|FlagMore) != 0 {
			return ErrInvalidFlags
		}
		// FlagFin belongs exclusively on the final HEADERS chunk (FlagMore == 0).
		if flags&(FlagFin|FlagMore) == (FlagFin | FlagMore) {
			return ErrInvalidFlags
		}
		// Non-final 0-byte HEADERS chunk is forbidden (anti-DoS).
		if payloadLen == 0 && flags&FlagMore != 0 {
			return ErrInvalidPayloadSize
		}
	case TypeData:
		if streamID == 0 {
			return ErrInvalidStreamID
		}
		if flags & ^FlagFin != 0 {
			return ErrInvalidFlags
		}
		if payloadLen == 0 && flags&FlagFin == 0 {
			return ErrInvalidPayloadSize
		}
	case TypeWSText, TypeWSBinary:
		if streamID == 0 {
			return ErrInvalidStreamID
		}
		if flags & ^FlagMsgFin != 0 {
			return ErrInvalidFlags
		}
		// Non-final 0-byte WS chunk is forbidden (anti-DoS); 0-byte WITH FlagMsgFin is valid (ws.send("")).
		if payloadLen == 0 && flags&FlagMsgFin == 0 {
			return ErrInvalidPayloadSize
		}
	case TypeWSClose:
		if streamID == 0 {
			return ErrInvalidStreamID
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen < 2 || payloadLen > MaxWSClosePayloadBytes {
			return ErrInvalidPayloadSize
		}
	default:
		return ErrInvalidFrameType
	}
	return nil
}

// EncodeHeader writes the 8-byte header into dst (must have len(dst) >= HeaderSize).
func EncodeHeader(dst []byte, streamID uint32, frameType, flags uint8, payloadLen int) error {
	if len(dst) < HeaderSize {
		return ErrFrameTooShort
	}
	if err := ValidateFrameMeta(streamID, frameType, flags, payloadLen); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(dst[0:4], streamID)
	dst[4] = frameType
	dst[5] = flags
	binary.BigEndian.PutUint16(dst[6:8], uint16(payloadLen)) // #nosec G115 -- ValidateFrameMeta bounds payloadLen <= 65535
	return nil
}

// Encode allocates and returns the full encoded wire bytes for Frame.
func (f *Frame) Encode() ([]byte, error) {
	if err := ValidateFrameMeta(f.StreamID, f.Type, f.Flags, len(f.Payload)); err != nil {
		return nil, err
	}
	if f.Type == TypeCredit {
		if _, err := DecodeCreditPayload(f.Payload); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	_ = EncodeHeader(buf[:HeaderSize], f.StreamID, f.Type, f.Flags, len(f.Payload))
	copy(buf[HeaderSize:], f.Payload)
	return buf, nil
}

// EncodeInto encodes Frame into dst (must have len >= HeaderSize+len(Payload)) without allocating.
func EncodeInto(dst []byte, streamID uint32, frameType, flags uint8, payload []byte) ([]byte, error) {
	total := HeaderSize + len(payload)
	if len(dst) < total {
		return nil, ErrPayloadTooLarge
	}
	if err := EncodeHeader(dst[:HeaderSize], streamID, frameType, flags, len(payload)); err != nil {
		return nil, err
	}
	if frameType == TypeCredit {
		if _, err := DecodeCreditPayload(payload); err != nil {
			return nil, err
		}
	}
	copy(dst[HeaderSize:total], payload)
	return dst[:total], nil
}

// Decode validates and decodes a wire packet into a Frame (Payload aliases raw[HeaderSize:]).
func Decode(raw []byte) (*Frame, error) {
	if len(raw) < HeaderSize {
		return nil, ErrFrameTooShort
	}
	streamID := binary.BigEndian.Uint32(raw[0:4])
	frameType := raw[4]
	flags := raw[5]
	declaredLen := int(binary.BigEndian.Uint16(raw[6:8]))
	if len(raw) != HeaderSize+declaredLen {
		return nil, ErrFrameLengthMismatch
	}
	if err := ValidateFrameMeta(streamID, frameType, flags, declaredLen); err != nil {
		return nil, err
	}
	payload := raw[HeaderSize:]
	if frameType == TypeCredit {
		if _, err := DecodeCreditPayload(payload); err != nil {
			return nil, err
		}
	}
	return &Frame{
		StreamID: streamID,
		Type:     frameType,
		Flags:    flags,
		Payload:  payload,
	}, nil
}

func EncodeCreditPayload(delta uint32) ([]byte, error) {
	if delta == 0 || delta > 0x7FFFFFFF {
		return nil, ErrInvalidCreditDelta
	}
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], delta)
	return out[:], nil
}

func DecodeCreditPayload(payload []byte) (uint32, error) {
	if len(payload) != 4 {
		return 0, ErrInvalidPayloadSize
	}
	delta := binary.BigEndian.Uint32(payload)
	if delta == 0 || delta > 0x7FFFFFFF {
		return 0, ErrInvalidCreditDelta
	}
	return delta, nil
}

func EncodeResetPayload(code ErrorCode) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], uint32(code))
	return out[:]
}

func DecodeResetPayload(payload []byte) (ErrorCode, error) {
	if len(payload) != 4 {
		return 0, ErrInvalidPayloadSize
	}
	return ErrorCode(binary.BigEndian.Uint32(payload)), nil
}

func EncodeGoAwayPayload(code ErrorCode) []byte {
	var out [4]byte
	binary.BigEndian.PutUint32(out[:], uint32(code))
	return out[:]
}

func DecodeGoAwayPayload(payload []byte) (ErrorCode, error) {
	if len(payload) != 4 {
		return 0, ErrInvalidPayloadSize
	}
	return ErrorCode(binary.BigEndian.Uint32(payload)), nil
}

func EncodePingPayload(seq uint64) []byte {
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], seq)
	return out[:]
}

// EncodeWSClosePayload builds a strictly valid UTF-8, bounded RFC 6455 close payload (2-byte code + <=123B reason).
func EncodeWSClosePayload(code uint16, reason string) []byte {
	clean := strings.ToValidUTF8(reason, "")
	rb := []byte(clean)
	if len(rb) > MaxWSClosePayloadBytes-2 {
		rb = truncateUTF8(rb, MaxWSClosePayloadBytes-2)
	}
	out := make([]byte, 2+len(rb))
	binary.BigEndian.PutUint16(out[0:2], code)
	copy(out[2:], rb)
	return out
}

func DecodeWSClosePayload(payload []byte) (code uint16, reason string, err error) {
	if len(payload) < 2 || len(payload) > MaxWSClosePayloadBytes {
		return 0, "", ErrInvalidPayloadSize
	}
	if !utf8.Valid(payload[2:]) {
		return 0, "", ErrInvalidPayloadSize
	}
	return binary.BigEndian.Uint16(payload[0:2]), string(payload[2:]), nil
}

func truncateUTF8(b []byte, maxLen int) []byte {
	if len(b) <= maxLen {
		return b
	}
	n := maxLen
	for n > 0 && (b[n]&0xC0) == 0x80 {
		n--
	}
	return b[:n]
}
