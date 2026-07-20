package handlers

import (
	"encoding/json"
	"errors"
	"io"
)

// ensureEOF verifies the request body contains a single JSON document.
func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return errors.New("extra data")
}
