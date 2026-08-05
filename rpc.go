package main

import "encoding/json"

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string, retryable bool) []byte {
	raw, _ := json.Marshal(envelope{
		OK: false,
		Error: &rpcError{
			Code:      code,
			Message:   message,
			Retryable: retryable,
		},
	})
	return raw
}
