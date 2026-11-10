package linkproto

import (
	"encoding/binary"
	"encoding/json"
	"io"
)

// Stream types: the first byte of every yamux stream.
const (
	StreamData    byte = 'D' // a raw TCP pipe to the connector's API on 127.0.0.1
	StreamCommand byte = 'C' // one signed command: frames Signed, stdin; answers stdout, SignedAnswer
	StreamControl byte = 'K' // the box's one stream toward the relay: Control messages
)

// MaxControlFrame bounds a control or envelope frame; stdin and stdout frames
// are bounded by MaxPayload.
const (
	MaxControlFrame = 64 << 10
	MaxPayload      = 1 << 20 // provision.exe's own stdin cap, and digi-mcp's output cap
)

// WriteFrame writes a 4-byte big-endian length and b.
func WriteFrame(w io.Writer, b []byte) error {
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if _, err := w.Write(n[:]); err != nil {
		return err
	}
	_, err := w.Write(b)
	return err
}

// ReadFrame reads one frame of at most max bytes.
func ReadFrame(r io.Reader, max int) ([]byte, error) {
	var n [4]byte
	if _, err := io.ReadFull(r, n[:]); err != nil {
		return nil, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if int64(size) > int64(max) {
		return nil, refuse(CodeFrameTooLarge)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}

// WriteJSONFrame and ReadJSONFrame carry one JSON value per frame.
func WriteJSONFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, b)
}

func ReadJSONFrame(r io.Reader, max int, v any) error {
	b, err := ReadFrame(r, max)
	if err != nil {
		return err
	}
	return strictUnmarshal(b, v)
}

// Control is a message on the box's control stream.
//
//	box → relay   {"t":"live?","epoch":n}            every LivenessInterval
//	relay → box   {"t":"live","current":true|false}  false: reconnect now (plan 083 §11 finding 6)
//	relay → box   {"t":"goaway","reason":"…"}        this relay is stopping: reconnect now, then drain
type Control struct {
	T       string `json:"t"`
	Epoch   int64  `json:"epoch,omitempty"`
	Current *bool  `json:"current,omitempty"`
	Relay   string `json:"relay,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const (
	CtlLiveQuery = "live?"
	CtlLive      = "live"
	CtlGoAway    = "goaway"
)
