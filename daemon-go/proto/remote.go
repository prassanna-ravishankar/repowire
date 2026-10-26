package proto

import "strings"

// Cross-machine identity (rfcs/cross-machine-circles.md).

// SharedCircle reports whether a circle spans the owner's machines. Circles a
// machine generates for itself stay local: empty, "*", "default", an all-digit
// name (an unnamed tmux session) and window-<digits>. The rule is lexical, so
// a circle deliberately named "7" is local too.
func SharedCircle(circle string) bool {
	switch {
	case circle == "", circle == "*", circle == "default", allDigits(circle):
		return false
	case strings.HasPrefix(circle, "window-") && allDigits(strings.TrimPrefix(circle, "window-")):
		return false
	}
	return true
}

func allDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

const remotePrefix = "remote:"

// RemotePeerID is how a peer on another machine appears in this machine's
// ask records. No local peer may claim a remote: id.
func RemotePeerID(machine, peerID string) PeerID {
	return PeerID(remotePrefix + machine + ":" + peerID)
}

// ParseRemotePeerID splits a remote: id into its machine and peer id.
func ParseRemotePeerID(id PeerID) (machine, peerID string, ok bool) {
	rest, ok := strings.CutPrefix(string(id), remotePrefix)
	if !ok {
		return "", "", false
	}
	machine, peerID, ok = strings.Cut(rest, ":")
	return machine, peerID, ok && machine != "" && peerID != ""
}

// IsRemotePeerID reports whether id names a peer on another machine.
func IsRemotePeerID(id PeerID) bool { return strings.HasPrefix(string(id), remotePrefix) }
