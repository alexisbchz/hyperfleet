package initd

import (
	"bytes"
	"encoding/json"
	"io"
)

// jsonBody returns a *bytes.Buffer (so the caller can set ContentLength
// without buffering twice). HTML escaping is off because the in-guest
// parser is intentionally minimal and doesn't decode \uXXXX — keeping `<`,
// `>`, `&` literal avoids a wire-format trap.
func jsonBody(v any) (*bytes.Buffer, error) {
	buf := new(bytes.Buffer)
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf, nil
}

func decodeJSON(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}
