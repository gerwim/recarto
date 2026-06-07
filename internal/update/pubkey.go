package update

// ManifestPublicKey is the minisign public key that signs the release manifest.
// The matching secret key lives only in CI (masked variable RECARTO_MINISIGN_KEY)
// and offline — never in this repo. Rotating it requires shipping a new binary.
const ManifestPublicKey = "RWRAMnGihrJ8EmiPipeK6yvoSTh0v7zpkW+/EOm++cSbnDmuHXZlDhiy"
