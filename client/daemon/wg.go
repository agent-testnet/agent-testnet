package daemon

// WGInterfaceName is the kernel name of the testnet WireGuard tunnel interface
// on the client host. testnet-client setup creates it (using the wg-quick
// config it writes to /etc/wireguard/<name>.conf) and the daemon then assumes
// it remains up across daemon restarts.
//
// Keep this in sync with anywhere that hardcodes the name; references should
// import this constant rather than duplicate the literal.
const WGInterfaceName = "wg-testnet"

// WGSystemConfigPath is the path wg-quick reads when invoked with just the
// interface name (e.g. `wg-quick up wg-testnet`). testnet-client setup writes
// this file, and the daemon re-uses it when restoring a missing tunnel.
const WGSystemConfigPath = "/etc/wireguard/" + WGInterfaceName + ".conf"
