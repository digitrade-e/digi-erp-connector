package provision

import "fmt"

// Refusal is why an operation did not do what it was asked, with the fix in
// one line. Code is stable (callers branch on it); Message and Fix are for a
// human.
type Refusal struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func (r *Refusal) Error() string { return r.Code + ": " + r.Message }

func refuse(code, fix, format string, args ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, args...), Fix: fix}
}

// The refusal codes. Each names one condition; the message says which value.
const (
	CodeSpecInvalid         = "spec_invalid"
	CodeChangeSetID         = "change_set_id"
	CodeLoopbackListen      = "loopback_listen"
	CodeSaLogin             = "sa_login"
	CodeImporterRequired    = "importer_required"
	CodeImporterMissing     = "importer_missing"
	CodeSendOrderDirMissing = "send_order_dir_missing"
	CodeAuthPasswordWeak    = "auth_password_weak"
	CodeFirewallAny         = "firewall_any"
	CodeConfigRefused       = "config_refused"
	CodeDrift               = "drift"
	CodeServiceMissing      = "service_missing"
	CodeOrdersPending       = "orders_pending"
	CodePendingUnknown      = "pending_unknown"
	CodeDBUnreachable       = "db_unreachable"
	CodeProceduresFailed    = "procedures_failed"
	CodeLeastPrivilege      = "least_privilege_failed"
	CodeSecretRoundTrip     = "secret_roundtrip"
	CodeWriteFailed         = "write_failed"
	CodeFirewallFailed      = "firewall_failed"
	CodeServiceFailed       = "service_failed"
	CodePortProofFailed     = "port_proof_failed"
	CodePreimageMissing     = "preimage_missing"
	CodeUnsupported         = "unsupported_os"
	CodeFirewallForeign     = "firewall_foreign"
	CodeOrderNumberConflict = "order_number_conflict"
	CodeBusy                = "busy"
	CodeDBHostPublic        = "db_host_public"
	CodeImporterWritable    = "importer_writable"
	CodeInternal            = "internal_error"
)
