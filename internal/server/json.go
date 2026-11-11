package server

import "encoding/json"

func mustJSON(v any) []byte {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return []byte("{}")
	}
	return b
}

func jsonUnmarshal(b []byte, v any) error {
	return json.Unmarshal(b, v)
}
