// Package main implements the AgentMecha sidecar: a dual-role A2A
// Server+Client built on the official a2a-go/v2 SDK.
//
// ndjson.go contains the NDJSON framing helpers used by the User Server
// client (userserver.go). The actual >64KB-safe scanner is the shared
// a2a.NewNDJSONScanner / a2a.NDJSONReader from pkg/a2a/stream.go; this file
// is the thin wrapper + typed record decoder that the sidecar imports.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	shared "local-agent-stack/pkg/a2a"
)

// NDJSONRecord is one line of the Sidecar <-> User Server internal protocol
// (contract §1.7).
type NDJSONRecord struct {
	Type       string         `json:"type"`
	TaskID     string         `json:"taskId,omitempty"`
	State      string         `json:"state,omitempty"`
	Progress   float64        `json:"progress,omitempty"`
	Text       string         `json:"text,omitempty"`
	ArtifactID string         `json:"artifactId,omitempty"`
	Code       string         `json:"code,omitempty"`
	Message    string         `json:"message,omitempty"`
	Extra      map[string]any `json:"-"`
}

// EncodeNDJSON serialises one record as a single line (newline terminated).
func EncodeNDJSON(v any) ([]byte, error) { return shared.NDJSONLine(v) }

// NDJSONScanner wraps shared.NewNDJSONScanner.
//
// 64KB FIX (contract §1.7 / E11): the standard bufio.Scanner has
// MaxScanTokenSize=64*1024 and fails with "bufio.Scanner: token too long"
// on a 128000-char artifact line. shared.NewNDJSONScanner calls
// scanner.Buffer(make([]byte, 64*1024), 10*1024*1024) so a single line up
// to 10MB is accepted. We NEVER use a default bufio.Scanner here.
func NDJSONScanner(r io.Reader) *bufio.Scanner { return shared.NewNDJSONScanner(r) }

// NDJSONLineReader is the unbounded alternative based on bufio.Reader.ReadBytes.
// It has NO token-size limit at all and is what userserver.go uses in the
// hot path (maximum safety for long:128000 artifacts).
type NDJSONLineReader struct {
	r *shared.NDJSONReader
}

// NewNDJSONLineReader wraps r in a large-buffered, limit-free line reader.
func NewNDJSONLineReader(r io.Reader) *NDJSONLineReader {
	return &NDJSONLineReader{r: shared.NewNDJSONReader(r)}
}

// Next returns the next raw line (including trailing '\n'). io.EOF at clean end.
func (n *NDJSONLineReader) Next() ([]byte, error) { return n.r.Next() }

// DecodeNDJSONRecord decodes one (possibly >64KB) line into NDJSONRecord.
func DecodeNDJSONRecord(line []byte) (NDJSONRecord, error) {
	line = bytes.TrimSpace(line)
	var rec NDJSONRecord
	if len(line) == 0 {
		return rec, io.EOF
	}
	if err := json.Unmarshal(line, &rec); err != nil {
		return rec, fmt.Errorf("ndjson decode: %w", err)
	}
	return rec, nil
}

// ReadNDJSONRecords reads every record from r using the 64KB-safe reader and
// invokes onRecord for each. It returns the number of records consumed.
func ReadNDJSONRecords(r io.Reader, onRecord func(NDJSONRecord) error) (int, error) {
	lr := NewNDJSONLineReader(r)
	count := 0
	for {
		line, err := lr.Next()
		if err != nil {
			if err == io.EOF {
				return count, nil
			}
			return count, err
		}
		rec, err := DecodeNDJSONRecord(line)
		if err != nil {
			if err == io.EOF {
				continue // blank line
			}
			return count, err
		}
		count++
		if onRecord != nil {
			if err := onRecord(rec); err != nil {
				return count, err
			}
		}
	}
}
