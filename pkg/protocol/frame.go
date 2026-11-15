package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// HeaderSize is the fixed size of a P2PMux frame header in bytes.
	HeaderSize = 10

	// MaxPayloadSize is the maximum allowed payload size for a single frame (64KB).
	// We recommend chunking at 16KB-32KB for SCTP transmission safety.
	MaxPayloadSize = 64 * 1024

	// RecommendedChunkSize is the default chunk size used for streaming data.
	RecommendedChunkSize = 32 * 1024
)

// Frame types
const (
	TypeData         byte = 0x01 // Carries stream payload
	TypeHeaders      byte = 0x02 // Request or response headers / metadata
	TypeRstStream    byte = 0x03 // Abort stream with error code
	TypeWindowUpdate byte = 0x04 // Flow control window update
	TypePing         byte = 0x05 // Keepalive ping
	TypePong         byte = 0x06 // Keepalive pong
	TypeWSConnect    byte = 0x10 // WebSocket handshake connect
	TypeWSData       byte = 0x11 // WebSocket data frame
	TypeWSClose      byte = 0x12 // WebSocket close
)

// Frame flags
const (
	FlagNone byte = 0x00
	FlagFin  byte = 0x01 // End of stream data
	FlagAck  byte = 0x02 // Acknowledgement
)

// Frame represents a single protocol multiplexing frame.
type Frame struct {
	StreamID uint32
	Type     byte
	Flags    byte
	Length   uint32
	Payload  []byte
}

var (
	ErrPayloadTooLarge = errors.New("frame payload exceeds maximum size")
	ErrInvalidHeader   = errors.New("invalid frame header")
	ErrBufferTooShort  = errors.New("buffer too short for frame header")
)

// NewFrame creates a new Frame with length automatically set.
func NewFrame(streamID uint32, frameType byte, flags byte, payload []byte) *Frame {
	return &Frame{
		StreamID: streamID,
		Type:     frameType,
		Flags:    flags,
		Length:   uint32(len(payload)),
		Payload:  payload,
	}
}

// EncodeHeader serializes the 10-byte header into dst.
func (f *Frame) EncodeHeader(dst []byte) error {
	if len(dst) < HeaderSize {
		return ErrBufferTooShort
	}
	binary.BigEndian.PutUint32(dst[0:4], f.StreamID)
	dst[4] = f.Type
	dst[5] = f.Flags
	binary.BigEndian.PutUint32(dst[6:10], uint32(len(f.Payload)))
	return nil
}

// Encode serializes the entire frame (header + payload) into a single byte slice.
func (f *Frame) Encode() []byte {
	buf := make([]byte, HeaderSize+len(f.Payload))
	_ = f.EncodeHeader(buf)
	if len(f.Payload) > 0 {
		copy(buf[HeaderSize:], f.Payload)
	}
	return buf
}

// DecodeHeader parses the 10-byte header from src.
func DecodeHeader(src []byte) (*Frame, error) {
	if len(src) < HeaderSize {
		return nil, ErrBufferTooShort
	}
	streamID := binary.BigEndian.Uint32(src[0:4])
	frameType := src[4]
	flags := src[5]
	length := binary.BigEndian.Uint32(src[6:10])

	if length > MaxPayloadSize {
		return nil, fmt.Errorf("%w: length %d > %d", ErrPayloadTooLarge, length, MaxPayloadSize)
	}

	return &Frame{
		StreamID: streamID,
		Type:     frameType,
		Flags:    flags,
		Length:   length,
	}, nil
}

// ReadFrame reads a complete frame from an io.Reader.
func ReadFrame(r io.Reader) (*Frame, error) {
	hdrBuf := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, hdrBuf); err != nil {
		return nil, err
	}

	frame, err := DecodeHeader(hdrBuf)
	if err != nil {
		return nil, err
	}

	if frame.Length > 0 {
		frame.Payload = make([]byte, frame.Length)
		if _, err := io.ReadFull(r, frame.Payload); err != nil {
			return nil, err
		}
	}
	return frame, nil
}

// DecodeFrameBytes decodes a complete frame from a byte slice (as received from WebRTC DataChannel message).
func DecodeFrameBytes(data []byte) (*Frame, error) {
	if len(data) < HeaderSize {
		return nil, ErrBufferTooShort
	}
	frame, err := DecodeHeader(data[:HeaderSize])
	if err != nil {
		return nil, err
	}

	if uint32(len(data)-HeaderSize) < frame.Length {
		return nil, io.ErrUnexpectedEOF
	}

	frame.Payload = data[HeaderSize : HeaderSize+int(frame.Length)]
	return frame, nil
}
