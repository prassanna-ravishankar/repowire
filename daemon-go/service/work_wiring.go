package service

// work_wiring.go holds the compile-time assertions that the concrete types main
// injects satisfy the narrow seams this area declares. If a sibling area changes
// a registry/spawn signature, these break the build here instead of silently at
// the main wiring site — fail loud, at compile time.

import (
	"github.com/repowire/repowire/daemon-go/peer"
)

var (
	// *peer.Registry satisfies the registry seam used by SessionControl
	// (resolve/get/getbypane/getall/unregister).
	_ controlRegistry = (*peer.Registry)(nil)

	// *SpawnService satisfies the spawn seam SessionControl drives (the SAME
	// shared instance main also hands to the spawn routes via WithSpawn).
	_ spawnExecutor = (*SpawnService)(nil)

	// *PeerDelivery satisfies the scheduled-ask opener the JobRunner dispatches
	// through (reply_delivery="pull").
	_ scheduledAskOpener = (*PeerDelivery)(nil)
)
