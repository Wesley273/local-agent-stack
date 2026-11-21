package a2a

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ---------------------------------------------------------------------------
// SSE reader — handles frames split across arbitrary TCP chunks (contract §7).
// A single "data:" frame may be delivered over many Reads; never assume one
// Read == one frame.
// ---------------------------------------------------------------------------

// SSEDecoder incrementally parses an SSE byte stream.
type SSEDecoder struct {
	buf    bytes.Buffer
	events []SSEEvent
	done   bool
}

// Feed adds bytes (possibly a partial frame) and returns any complete events.
func (d *SSEDecoder) Feed(chunk []byte) []SSEEvent {
	d.buf.Write(chunk)
	d.events = d.events[:0]
	for {
		// A frame ends with a blank line (\n\n or \r\n\r\n).
		raw := d.buf.Bytes()
		idx := bytes.Index(raw, []byte("\n\n"))
		sep := 2
		if idx < 0 {
			idx = bytes.Index(raw, []byte("\r\n\r\n"))
			sep = 4
		}
		if idx < 0 {
			break
		}
		block := string(raw[:idx])
		d.buf.Next(idx + sep)
		ev := parseSSEBlock(block)
		if ev != nil {
			d.events = append(d.events, *ev)
			if strings.TrimSpace(ev.Data) == "[DONE]" {
				d.done = true
			}
		}
	}
	return append([]SSEEvent(nil), d.events...)
}

// Done reports whether a [DONE] frame has been observed.
func (d *SSEDecoder) Done() bool { return d.done }

func parseSSEBlock(block string) *SSEEvent {
	var ev SSEEvent
	var dataLines []string
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "event:") {
			ev.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		// id:, retry: — ignored for this contract.
	}
	if len(dataLines) == 0 {
		return nil
	}
	ev.Data = strings.Join(dataLines, "\n")
	return &ev
}

// ReadSSEStream consumes r until EOF/[DONE] and invokes onEvent for each frame.
// It is the SDK-side reference implementation.
func ReadSSEStream(r io.Reader, onEvent func(SSEEvent) bool) error {
	dec := &SSEDecoder{}
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, ev := range dec.Feed(buf[:n]) {
				if !onEvent(ev) {
					return nil
				}
				if dec.Done() {
					return nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// ---------------------------------------------------------------------------
// NDJSON reader — the 64KB fix (contract §1.7, resume bug #2)
// ---------------------------------------------------------------------------

const (
	// DefaultScannerBuf is the initial buffer (stdlib default).
	DefaultScannerBuf = 64 * 1024
	// MaxNDJSONLine is the maximum accepted single line: 10 MB.
	MaxNDJSONLine = 10 * 1024 * 1024
)

var ErrTokenTooLong = errors.New("ndjson line exceeded 10MB limit")

// NewNDJSONScanner returns a scanner SAFE for large artifact lines.
//
// BUG being fixed (from resume): the default bufio.Scanner has
// MaxScanTokenSize = 64*1024 and fails with "bufio.Scanner: token too long"
// on a 128KB TaskArtifactUpdateEvent line, which silently kills the stream.
// Fix: scanner.Buffer(make([]byte, 64*1024), 10*1024*1024).
func NewNDJSONScanner(r io.Reader) *bufio.Scanner {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, DefaultScannerBuf), MaxNDJSONLine)
	return s
}

// ReadNDJSON reads all NDJSON records, optionally invoking onRecord per line.
// Lines exceeding MaxNDJSONLine produce ErrTokenTooLong (never a silent hang).
func ReadNDJSON(r io.Reader, onRecord func([]byte) error) (int, error) {
	s := NewNDJSONScanner(r)
	count := 0
	for s.Scan() {
		line := s.Bytes()
		count++
		if onRecord != nil {
			if err := onRecord(line); err != nil {
				return count, err
			}
		}
	}
	if err := s.Err(); err != nil {
		if strings.Contains(err.Error(), "token too long") {
			return count, ErrTokenTooLong
		}
		return count, err
	}
	return count, nil
}

// ReadNDJSONMap is a typed convenience wrapper.
func ReadNDJSONMap(r io.Reader, onRecord func(map[string]any) error) (int, error) {
	return ReadNDJSON(r, func(line []byte) error {
		var m map[string]any
		if err := json.Unmarshal(line, &m); err != nil {
			return fmt.Errorf("ndjson decode: %w", err)
		}
		return onRecord(m)
	})
}

// NDJSONReader is an alternative implementation using bufio.Reader.ReadBytes,
// which has NO token-size limit at all. Used by the sidecar for maximum safety.
type NDJSONReader struct {
	r *bufio.Reader
}

// NewNDJSONReader wraps r with a large buffer.
func NewNDJSONReader(r io.Reader) *NDJSONReader {
	return &NDJSONReader{r: bufio.NewReaderSize(r, DefaultScannerBuf)}
}

// Next returns the next record including the trailing newline.
// Returns io.EOF at clean end of stream.
func (n *NDJSONReader) Next() ([]byte, error) {
	line, err := n.r.ReadBytes('\n')
	if len(line) > 0 {
		return line, nil
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		return nil, err
	}
	return line, nil
}
