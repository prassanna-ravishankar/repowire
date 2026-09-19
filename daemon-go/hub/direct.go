package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

type routeError struct {
	status int
	detail any
}

// Error renders structured details as JSON so non-HTTP callers (MCP tools)
// see the same fields the HTTP body carries, not Go's map formatting.
func (e *routeError) Error() string {
	switch e.detail.(type) {
	case string:
		return e.detail.(string)
	case map[string]any, []any:
		if raw, err := json.Marshal(e.detail); err == nil {
			return string(raw)
		}
	}
	return fmt.Sprint(e.detail)
}

func routeErr(status int, detail any) error { return &routeError{status: status, detail: detail} }

func writeRouteError(w http.ResponseWriter, err error) {
	var re *routeError
	if errors.As(err, &re) {
		writeJSONError(w, re.status, re.detail)
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}
