# Third-party notices

The task and account concepts and the official Codex wakeup request shape are behaviorally informed by
[`jlcodes99/cockpit-tools`](https://github.com/jlcodes99/cockpit-tools),
commit `cdcd27c39b0d4433a445b5ac55bd50fbd59eddf8`. No source code from that
project is copied here. In particular, the reference was inspected for its
official OAuth Responses URL, headers, JSON fields, streaming, and completion
handling; this plugin reimplements those decisions independently.

The implementation also follows the public CLIProxyAPI v7 native-plugin ABI
and the public host callback shapes. The direct Codex request shape and
defensive auth-material handling were independently implemented with the
behavior of `Cody292/quota-activation`, commit `ee5c6e134e68`, as a reference.
No source code from that project is copied here.

The CPA ABI was checked against the local CLIProxyAPI source at tag `v7.2.151`,
commit `5208aec703b5ce7e3445f6e9d91cc13b3e78003a`. In particular, the checked
contracts are `sdk/pluginabi/types.go` (`ABIVersion=1`, `SchemaVersion=5` and
the plugin/host method names), `sdk/pluginapi/types.go` (`Metadata`,
`ConfigField`, `ManagementResponse`, `ManagementRoute`, `ResourceRoute`,
`HTTPRequest`, `HTTPResponse`, `HostAuthFileEntry`, and auth get/list shapes),
`internal/pluginhost/rpc_schema.go` (the base64 JSON encoding of
`ConfigYAML []byte` and management response wire), and
`internal/pluginhost/management.go` (route normalization, handler injection,
management authentication, and `/v0/resource/plugins/<id>` dispatch).

The local source was used only as an API compatibility reference; no
CLIProxyAPI source is bundled or copied into this plugin.

This project has no runtime telemetry and never persists access tokens,
refresh tokens, Authorization headers, or complete upstream responses.
