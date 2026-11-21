package main

import (
	"encoding/json"
	"io"
)

func ioReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(io.LimitReader(r, 32<<20)) }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}
