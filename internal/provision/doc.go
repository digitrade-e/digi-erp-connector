// Package provision configures this machine's connector without the GUI —
// what digi-erp-connector-provision.exe runs (plan 068: digi-mcp drives it
// over WinRM, with fixed command lines only).
//
// Four operations, JSON in and JSON out:
//
//   - Discover reads the box: SQL Server instances and their ports, the
//     connector's service and config, the network, and — given a database
//     credential — which databases look like an ERP. It writes nothing.
//   - Apply makes the connector match a Spec: config.yaml, the DB secret, the
//     Hasavshevet procedures, an optional least-privilege SQL login, a
//     self-signed TLS pair, the firewall rule, the service's dependencies and
//     recovery. It restarts the service only when the order queue is empty and
//     proves the port afterwards; if the proof fails it puts everything back.
//     A second identical Apply changes nothing.
//   - Verify checks what is running against what is configured.
//   - Restore puts a change set's pre-image back.
//
// Rules that hold throughout:
//   - Secrets arrive on stdin only (Spec), never on a command line, and never
//     leave: no result, log line or pre-image listing carries one (Redact is
//     the last line of defence over every result).
//   - Nothing is written before the whole Spec is valid and the target config
//     passes the daemon's own startup checks (api.CheckConfig).
//   - Every file goes through atomicfile.Write; the data directory gets the
//     daemon's DACL before and after (dataacl).
//   - The SQL it runs is fixed text; the only values written into it are a
//     database name that passed hasavshevet.IsSafeDBName and a password from
//     NewSQLPassword's alphabet.
package provision
