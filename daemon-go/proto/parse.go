package proto

import "encoding/json"

// ParseEnvelope decodes only the "type" discriminator from a raw wire frame,
// leaving the concrete payload for a second unmarshal into the matching frame.
func ParseEnvelope(raw []byte) (FrameType, error) {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", err
	}
	return env.Type, nil
}
