package proto

import (
	_ "embed"
	"encoding/json"
	"time"
)

// RetentionSpec is the retention contract the daemon and the hosted runtime
// share (rfcs/retention.md). Content is text a peer or the human wrote;
// records are the rows saying who did what, when and how it ended; nothing
// younger than the ask lifetime expires, since an open ask still reads it.
// Cross-machine records keep a fixed window: it is protocol state, which no
// policy shortens.
type RetentionSpec struct {
	ContentDays      float64 `json:"content_days"`
	RecordsDays      float64 `json:"records_days"`
	AskLifeHours     float64 `json:"ask_life_hours"`
	RemoteWindowDays float64 `json:"remote_window_days"`
}

// retention.json is the one copy of the defaults: hosted/src/retention.ts
// imports the same file.
//
//go:embed retention.json
var retentionJSON []byte

// RetentionDefaults is what applies when an owner set nothing.
var RetentionDefaults = func() RetentionSpec {
	var d RetentionSpec
	if err := json.Unmarshal(retentionJSON, &d); err != nil {
		panic("proto: retention.json: " + err.Error())
	}
	return d
}()

// RemoteWindow is the fixed life of a cross-machine record.
var RemoteWindow = time.Duration(RetentionDefaults.RemoteWindowDays * float64(24*time.Hour))
