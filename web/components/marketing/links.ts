// Shared external link targets for the marketing site.
//
// The hosted dashboard is served by the relay at relay.repowire.io, not at
// repowire.io/dashboard (that path is the local daemon's dashboard). Marketing
// links point straight at the relay so visitors don't hit the local-style path
// and rely on a client-side hostname redirect.
export const RELAY_DASHBOARD_URL = "https://relay.repowire.io/dashboard";

export const GITHUB_URL = "https://github.com/prassanna-ravishankar/repowire";
export const DOCS_URL = "https://docs.repowire.io";
export const RELEASES_URL = `${GITHUB_URL}/releases`;

// Install commands, as the README's quickstart gives them.
export const BREW_INSTALL = "brew install prassanna-ravishankar/repowire/repowire";
export const CURL_INSTALL = "curl -fsSL https://github.com/prassanna-ravishankar/repowire/releases/latest/download/install.sh | sh";
