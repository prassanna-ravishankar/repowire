package hub

import (
	"errors"
	"fmt"
	"net/http"
)

type routeError struct {
	status int
	detail any
}

func (e *routeError) Error() string { return fmt.Sprint(e.detail) }

func routeErr(status int, detail any) error { return &routeError{status: status, detail: detail} }

func writeRouteError(w http.ResponseWriter, err error) {
	var re *routeError
	if errors.As(err, &re) {
		writeJSONError(w, re.status, re.detail)
		return
	}
	writeJSONError(w, http.StatusInternalServerError, err.Error())
}
