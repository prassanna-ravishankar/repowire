// Shared external link targets for the marketing site.
//
// The hosted dashboard is served by the relay at relay.repowire.io, not at
// repowire.io/dashboard (that path is the local daemon's dashboard). Marketing
// links point straight at the relay so visitors don't hit the local-style path
// and rely on a client-side hostname redirect.
export const RELAY_DASHBOARD_URL = "https://relay.repowire.io/dashboard";
