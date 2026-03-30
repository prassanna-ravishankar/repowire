"""Custom error types for Repowire protocol."""


class PeerDisconnectedError(Exception):
    """Raised when a peer disconnects during a pending query."""

    def __init__(self, peer_name: str):
        self.peer_name = peer_name
        super().__init__(f"Peer '{peer_name}' disconnected")


class DaemonConnectionError(Exception):
    """Raised when the Repowire daemon is not reachable."""

    def __init__(self) -> None:
        super().__init__(
            "Repowire daemon is not reachable. Start it with 'repowire serve'."
        )


class DaemonHTTPError(Exception):
    """Raised when the daemon returns an HTTP error response."""

    def __init__(self, status: int, text: str) -> None:
        self.status = status
        super().__init__(f"Daemon error {status}: {text}")


class DaemonTimeoutError(Exception):
    """Raised when a request to the daemon times out."""

    def __init__(self) -> None:
        super().__init__(
            "Daemon request timed out. The daemon may be overloaded or unreachable."
        )
