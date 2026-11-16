package protocol

import (
	"bytes"
	"crypto/rand"
	"errors"
	"reflect"
	"testing"
)

func TestFrameEncodeDecode(t *testing.T) {
	payload := []byte("hello webrtc p2pmux protocol")
	frame := NewFrame(42, TypeData, FlagFin, payload)

	encoded := frame.Encode()
	if len(encoded) != HeaderSize+len(payload) {
		t.Fatalf("expected length %d, got %d", HeaderSize+len(payload), len(encoded))
	}

	decoded, err := DecodeFrameBytes(encoded)
	if err != nil {
		t.Fatalf("unexpected decode error: %v", err)
	}

	if decoded.StreamID != 42 {
		t.Errorf("expected StreamID 42, got %d", decoded.StreamID)
	}
	if decoded.Type != TypeData {
		t.Errorf("expected Type %d, got %d", TypeData, decoded.Type)
	}
	if decoded.Flags != FlagFin {
		t.Errorf("expected Flags %d, got %d", FlagFin, decoded.Flags)
	}
	if decoded.Length != uint32(len(payload)) {
		t.Errorf("expected Length %d, got %d", len(payload), decoded.Length)
	}
	if !reflect.DeepEqual(decoded.Payload, payload) {
		t.Errorf("payload mismatch: expected %q, got %q", payload, decoded.Payload)
	}
}

func TestFrame_ZeroBytePayload(t *testing.T) {
	// Boundary check: Frame with 0-byte payload (e.g. pure FIN or PING)
	frame := NewFrame(101, TypeData, FlagFin, nil)
	encoded := frame.Encode()
	if len(encoded) != HeaderSize {
		t.Fatalf("expected length %d for zero payload frame, got %d", HeaderSize, len(encoded))
	}

	decoded, err := DecodeFrameBytes(encoded)
	if err != nil {
		t.Fatalf("Decode zero payload failed: %v", err)
	}
	if decoded.Length != 0 || len(decoded.Payload) != 0 {
		t.Errorf("expected 0 length payload, got %d", decoded.Length)
	}
	if decoded.Flags != FlagFin {
		t.Errorf("expected FlagFin, got %v", decoded.Flags)
	}
}

func TestFrame_MaxPayloadSize(t *testing.T) {
	// Boundary check: Payload exactly at MaxPayloadSize (64KB)
	payload := make([]byte, MaxPayloadSize)
	_, _ = rand.Read(payload)

	frame := NewFrame(999, TypeData, FlagNone, payload)
	encoded := frame.Encode()

	decoded, err := DecodeFrameBytes(encoded)
	if err != nil {
		t.Fatalf("Decode exactly MaxPayloadSize failed: %v", err)
	}
	if decoded.Length != MaxPayloadSize || len(decoded.Payload) != MaxPayloadSize {
		t.Fatalf("expected payload len %d, got %d", MaxPayloadSize, len(decoded.Payload))
	}
	if !bytes.Equal(decoded.Payload, payload) {
		t.Fatalf("MaxPayloadSize payload mismatch")
	}
}

func TestFrame_OversizedPayloadError(t *testing.T) {
	// Boundary check: Header declaring payload length > MaxPayloadSize (e.g. 64KB + 1)
	headerBuf := make([]byte, HeaderSize)
	f := NewFrame(1, TypeData, FlagNone, nil)
	_ = f.EncodeHeader(headerBuf)

	// Artificially corrupt the length field in the header to 65537
	headerBuf[6] = 0x00
	headerBuf[7] = 0x01
	headerBuf[8] = 0x00
	headerBuf[9] = 0x01 // 65537 > 65536

	_, err := DecodeHeader(headerBuf)
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("expected ErrPayloadTooLarge, got %v", err)
	}
}

func TestFrame_TruncatedBufferError(t *testing.T) {
	// Header declares 100 bytes payload, but only 20 bytes provided
	frame := NewFrame(1, TypeData, FlagNone, bytes.Repeat([]byte("X"), 100))
	encoded := frame.Encode()

	// Truncate to HeaderSize + 20
	truncated := encoded[:HeaderSize+20]
	_, err := DecodeFrameBytes(truncated)
	if !errors.Is(err, ErrInvalidFrameLength) && !errors.Is(err, ErrBufferTooShort) {
		t.Fatalf("expected ErrInvalidFrameLength or ErrBufferTooShort, got %v", err)
	}
}

func TestFrame_ExtraTrailingBytesError(t *testing.T) {
	// Header declares 10 bytes payload, but buffer provides 29 bytes (19 trailing garbage bytes)
	frame := NewFrame(1, TypeData, FlagNone, []byte("0123456789"))
	encoded := frame.Encode()
	corrupted := append(encoded, []byte("extra_trailing_bytes")...)

	_, err := DecodeFrameBytes(corrupted)
	if !errors.Is(err, ErrInvalidFrameLength) {
		t.Fatalf("expected ErrInvalidFrameLength on trailing bytes, got %v", err)
	}
}

func TestFrame_AllTypesAndFlags(t *testing.T) {
	types := []byte{
		TypeData, TypeHeaders, TypeRstStream, TypeWindowUpdate,
		TypePing, TypePong, TypeWSConnect, TypeWSData, TypeWSClose,
	}
	flags := []byte{FlagNone, FlagFin, FlagAck, FlagFin | FlagAck}

	for _, ty := range types {
		for _, fl := range flags {
			f := NewFrame(12345, ty, fl, []byte("type_test"))
			enc := f.Encode()
			dec, err := DecodeFrameBytes(enc)
			if err != nil {
				t.Fatalf("type %d flag %d decode error: %v", ty, fl, err)
			}
			if dec.Type != ty || dec.Flags != fl {
				t.Fatalf("mismatch: expected (type=%d, flag=%d), got (type=%d, flag=%d)", ty, fl, dec.Type, dec.Flags)
			}
		}
	}
}

func TestFrameReadWrite(t *testing.T) {
	payload := bytes.Repeat([]byte("A"), 1024)
	frame := NewFrame(1001, TypeHeaders, FlagNone, payload)

	var buf bytes.Buffer
	buf.Write(frame.Encode())

	readFrame, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame failed: %v", err)
	}

	if readFrame.StreamID != 1001 || readFrame.Type != TypeHeaders || readFrame.Length != 1024 {
		t.Fatalf("frame mismatch: %+v", readFrame)
	}
	if !bytes.Equal(readFrame.Payload, payload) {
		t.Fatalf("payload content mismatch")
	}
}

func TestFrameDecodeErrors(t *testing.T) {
	shortData := []byte{0x00, 0x00, 0x00}
	if _, err := DecodeFrameBytes(shortData); err != ErrBufferTooShort {
		t.Fatalf("expected ErrBufferTooShort, got %v", err)
	}
}
