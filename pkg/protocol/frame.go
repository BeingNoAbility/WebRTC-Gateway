package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// ProtocolVersion is the P2PMux V7 wire protocol version.
	ProtocolVersion = 7

	// HeaderSize is the fixed 8-byte wire frame header:
	// [0:4] Gen    (uint32 BE: 0 on p2pctrl, monotonic >=1 per HTTP pool DC, strictly 1 on p2pws)
	// [4]   Type   (uint8)
	// [5]   Flags  (uint8)
	// [6:8] Length (uint16 BE)
	HeaderSize = 8

	// MaxPayloadSize is the maximum payload expressible in uint16 Length.
	MaxPayloadSize = 65535

	// DefaultChunkSize aligns a single wire frame cleanly to 32 KiB (32760 + 8 = 32768).
	DefaultChunkSize = 32760

	// MinChunkSize prevents degenerate fragmentation on constrained SCTP peers.
	MinChunkSize = 1024

	// MinNegotiatedMaxMsgSize is the minimum SCTP maxMessageSize floor supported by V7.
	MinNegotiatedMaxMsgSize = 4096

	// MaxTotalHeadersSize caps an assembled HEADERS + CONTINUATION block (64 KiB).
	MaxTotalHeadersSize = 64 * 1024

	// MaxContinuationFrames caps CONTINUATION frames after HEADERS (supports 64 KiB even at 4 KiB MMS).
	MaxContinuationFrames = 16

	// MaxControlJSONSize caps control-plane JSON envelopes (SETTINGS/AUTH/CAP).
	MaxControlJSONSize = 32 * 1024

	// MaxWSMessageBytes is the maximum reassembled WebSocket message size (16 MiB).
	MaxWSMessageBytes = 16 * 1024 * 1024

	// MaxWSClosePayloadBytes enforces RFC 6455 close frame limit (2-byte code + <=123-byte reason).
	MaxWSClosePayloadBytes = 125

	// Pre-negotiated DataChannel layout: 1 Control DC (id=0) + 32 Reusable HTTP Pool DCs (id=1..32).
	// WebSocket streams open dedicated dynamic DCEP DataChannels labeled "p2pws".
	ControlChannelID    uint16 = 0
	ControlChannelLabel        = "p2pctrl"
	DefaultPoolSize            = 32
	WSChannelLabel             = "p2pws"
	DefaultMaxWSConns          = 64
)

// Control-plane frame types (exclusively allowed on p2pctrl, id=0, Gen == 0).
// Type 0x07 is reserved.
const (
	TypeSettings      uint8 = 0x01 // JSON
	TypeAuthChallenge uint8 = 0x02 // JSON
	TypeAuthResponse  uint8 = 0x03 // JSON
	TypeAuthResult    uint8 = 0x04 // JSON
	TypeCapReq        uint8 = 0x05 // JSON (mint / list / revoke)
	TypeCapResp       uint8 = 0x06 // JSON
	TypePing          uint8 = 0x08 // strictly 8-byte payload
	TypePong          uint8 = 0x09 // strictly 8-byte payload
	TypeGoAway        uint8 = 0x0A // strictly 4-byte ErrorCode
)

// Stream data-channel frame types (in-band on p2p_01..32 & p2pws, Gen >= 1).
const (
	TypeHeaders      uint8 = 0x10 // Flags: FlagFin | FlagMoreHeaders
	TypeContinuation uint8 = 0x11 // Flags: FlagMoreHeaders ONLY (0B with FlagMoreHeaders forbidden)
	TypeData         uint8 = 0x12 // Flags: FlagFin (HTTP body)
	TypeCredit       uint8 = 0x13 // strictly 4-byte delta (1..0x7FFFFFFF), Flags == 0
	TypeReset        uint8 = 0x14 // strictly 4-byte ErrorCode, Flags == 0
)

// Streaming WebSocket sub-frame types (exclusively on dedicated "p2pws" DCs, Gen == 1).
const (
	TypeWSText   uint8 = 0x21 // Flags: FlagMsgFin (first/only chunk of text message)
	TypeWSBinary uint8 = 0x22 // Flags: FlagMsgFin (first/only chunk of binary message)
	TypeWSCont   uint8 = 0x23 // Flags: FlagMsgFin (0B with FlagMsgFin == 0 forbidden)
	TypeWSClose  uint8 = 0x24 // 2..125 bytes: [u16 code][utf8 reason], Flags == 0
)

// Frame flags.
const (
	FlagNone        uint8 = 0x00
	FlagFin         uint8 = 0x01 // Half-close HTTP request/response direction
	FlagMsgFin      uint8 = 0x02 // Final chunk of a streaming WebSocket message
	FlagMoreHeaders uint8 = 0x04 // Additional CONTINUATION frame follows
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
	ErrInvalidGen          = errors.New("protocol: invalid stream generation for frame type")
	ErrInvalidPayloadSize  = errors.New("protocol: invalid payload size for frame type")
	ErrInvalidCreditDelta  = errors.New("protocol: credit delta must be in [1, 0x7fffffff]")
)

// PoolChannelID returns the pre-negotiated SCTP DataChannel ID for pool slot i (0-based -> id 1..32).
func PoolChannelID(slotIdx int) uint16 {
	if slotIdx < 0 || slotIdx >= 0xFFFF {
		return 0
	}
	return uint16(1 + slotIdx) // #nosec G115 -- bounded to [1, 0xFFFF]
}

// PoolChannelLabel returns the canonical DataChannel label for pool slot i (0-based -> "p2p_01".."p2p_32").
func PoolChannelLabel(slotIdx int) string {
	return fmt.Sprintf("p2p_%02d", slotIdx+1)
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

// IsPoolDataFrameType reports whether frameType is valid on an HTTP pool DC (p2p_01..32).
func IsPoolDataFrameType(frameType uint8) bool {
	return frameType >= TypeHeaders && frameType <= TypeReset
}

// IsWSDataFrameType reports whether frameType is valid on a dedicated "p2pws" DC.
func IsWSDataFrameType(frameType uint8) bool {
	return (frameType >= TypeHeaders && frameType <= TypeReset && frameType != TypeData) ||
		(frameType >= TypeWSText && frameType <= TypeWSClose)
}

// Frame is a decoded V7 wire frame.
type Frame struct {
	Gen     uint32
	Type    uint8
	Flags   uint8
	Payload []byte
}

// ValidateFrameMeta checks structural invariants shared by Encode and Decode.
func ValidateFrameMeta(gen uint32, frameType, flags uint8, payloadLen int) error {
	if payloadLen < 0 || payloadLen > MaxPayloadSize {
		return ErrPayloadTooLarge
	}
	switch frameType {
	case TypeSettings, TypeAuthChallenge, TypeAuthResponse, TypeAuthResult,
		TypeCapReq, TypeCapResp:
		if gen != 0 {
			return ErrInvalidGen
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen > MaxControlJSONSize {
			return ErrInvalidPayloadSize
		}
	case TypePing, TypePong:
		if gen != 0 {
			return ErrInvalidGen
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 8 {
			return ErrInvalidPayloadSize
		}
	case TypeGoAway:
		if gen != 0 {
			return ErrInvalidGen
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 4 {
			return ErrInvalidPayloadSize
		}
	case TypeCredit, TypeReset:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags != FlagNone {
			return ErrInvalidFlags
		}
		if payloadLen != 4 {
			return ErrInvalidPayloadSize
		}
	case TypeHeaders:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags & ^(FlagFin|FlagMoreHeaders) != 0 {
			return ErrInvalidFlags
		}
	case TypeContinuation:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags & ^FlagMoreHeaders != 0 {
			return ErrInvalidFlags
		}
		// V7 anti-DoS rule: non-final 0-byte CONTINUATION is strictly forbidden.
		if payloadLen == 0 && flags&FlagMoreHeaders != 0 {
			return ErrInvalidPayloadSize
		}
	case TypeData:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags & ^FlagFin != 0 {
			return ErrInvalidFlags
		}
	case TypeWSText, TypeWSBinary:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags & ^FlagMsgFin != 0 {
			return ErrInvalidFlags
		}
	case TypeWSCont:
		if gen == 0 {
			return ErrInvalidGen
		}
		if flags & ^FlagMsgFin != 0 {
			return ErrInvalidFlags
		}
		// V7 anti-DoS rule: non-final 0-byte WS_CONT is strictly forbidden.
		if payloadLen == 0 && flags&FlagMsgFin == 0 {
			return ErrInvalidPayloadSize
		}
	case TypeWSClose:
		if gen == 0 {
			return ErrInvalidGen
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
func EncodeHeader(dst []byte, gen uint32, frameType, flags uint8, payloadLen int) error {
	if len(dst) < HeaderSize {
		return ErrFrameTooShort
	}
	if err := ValidateFrameMeta(gen, frameType, flags, payloadLen); err != nil {
		return err
	}
	binary.BigEndian.PutUint32(dst[0:4], gen)
	dst[4] = frameType
	dst[5] = flags
	binary.BigEndian.PutUint16(dst[6:8], uint16(payloadLen)) // #nosec G115 -- ValidateFrameMeta bounds payloadLen to [0, MaxPayloadSize] (<= 65535)
	return nil
}

// Encode allocates and returns the full encoded wire bytes for Frame.
func (f *Frame) Encode() ([]byte, error) {
	if err := ValidateFrameMeta(f.Gen, f.Type, f.Flags, len(f.Payload)); err != nil {
		return nil, err
	}
	if f.Type == TypeCredit {
		if _, err := DecodeCreditPayload(f.Payload); err != nil {
			return nil, err
		}
	}
	buf := make([]byte, HeaderSize+len(f.Payload))
	_ = EncodeHeader(buf[:HeaderSize], f.Gen, f.Type, f.Flags, len(f.Payload))
	copy(buf[HeaderSize:], f.Payload)
	return buf, nil
}

// EncodeInto encodes Frame into dst (must have len >= HeaderSize+len(Payload)) without allocating.
func EncodeInto(dst []byte, gen uint32, frameType, flags uint8, payload []byte) ([]byte, error) {
	total := HeaderSize + len(payload)
	if len(dst) < total {
		return nil, ErrPayloadTooLarge
	}
	if err := EncodeHeader(dst[:HeaderSize], gen, frameType, flags, len(payload)); err != nil {
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
	gen := binary.BigEndian.Uint32(raw[0:4])
	frameType := raw[4]
	flags := raw[5]
	declaredLen := int(binary.BigEndian.Uint16(raw[6:8]))
	if len(raw) != HeaderSize+declaredLen {
		return nil, ErrFrameLengthMismatch
	}
	if err := ValidateFrameMeta(gen, frameType, flags, declaredLen); err != nil {
		return nil, err
	}
	payload := raw[HeaderSize:]
	if frameType == TypeCredit {
		if _, err := DecodeCreditPayload(payload); err != nil {
			return nil, err
		}
	}
	return &Frame{
		Gen:     gen,
		Type:    frameType,
		Flags:   flags,
		Payload: payload,
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

// EncodeWSClosePayload builds a strictly bounded RFC 6455 close payload (2-byte code + <=123B UTF-8 reason).
func EncodeWSClosePayload(code uint16, reason string) []byte {
	rb := []byte(reason)
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
