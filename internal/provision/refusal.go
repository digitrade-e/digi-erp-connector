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
	CodeSpecInvalid    = "spec_invalid"
	CodeChangeSetID    = "change_set_id"
	CodeLoopbackListen = "loopback_listen"
	// Link mode (plan 083 §8 A16).
	CodeSaLinkMode           = "sa_link_mode"
	CodeImporterNotERP       = "importer_not_erp"
	CodeImageFolderForbidden = "image_folder_forbidden"
	CodeSaLogin              = "sa_login"
	CodeImporterRequired     = "importer_required"
	CodeImporterMissing      = "importer_missing"
	CodeSendOrderDirMissing  = "send_order_dir_missing"
	CodeImageFolderMissing   = "image_folder_missing"
	CodeAuthPasswordWeak     = "auth_password_weak"
	CodeFirewallAny          = "firewall_any"
	CodeConfigRefused        = "config_refused"
	CodeDrift                = "drift"
	CodeServiceMissing       = "service_missing"
	CodeOrdersPending        = "orders_pending"
	CodePendingUnknown       = "pending_unknown"
	CodeDBUnreachable        = "db_unreachable"
	CodeProceduresFailed     = "procedures_failed"
	CodeLeastPrivilege       = "least_privilege_failed"
	CodeSecretRoundTrip      = "secret_roundtrip"
	CodeWriteFailed          = "write_failed"
	CodeFirewallFailed       = "firewall_failed"
	CodeServiceFailed        = "service_failed"
	CodePortProofFailed      = "port_proof_failed"
	CodePreimageMissing      = "preimage_missing"
	CodeUnsupported          = "unsupported_os"
	CodeFirewallForeign      = "firewall_foreign"
	CodeOrderNumberConflict  = "order_number_conflict"
	CodeBusy                 = "busy"
	CodeDBHostPublic         = "db_host_public"
	CodeImporterWritable     = "importer_writable"
	CodeInternal             = "internal_error"
)

// Plan 086 B1's codes.
const (
	CodeDependencyMissing  = "dependency_missing"  // service: a dependency that is not a service on this machine
	CodeDependencyDisabled = "dependency_disabled" // service: a dependency whose start type is Disabled (review m1)
	// CodeFirewallUnrestorable: firewall-remove, a rule of ours that is not one inbound TCP allow on one port, which
	// restore could not re-create (review m9).
	CodeFirewallUnrestorable = "firewall_unrestorable"
	CodeReceiptStoreExists   = "receipt_store_exists" // receipt-seed: the store exists (a seed is written once)
	CodeReceiptSeedLow       = "receipt_seed_low"     // receipt-seed: at or below a receipt already written
	CodeReceiptDirMissing    = "receipt_dir_missing"  // receipts: sendReceiptDir is not a folder here

	// plan 087: the counters and documents commands
	CodeCounterUnknown   = "counter_unknown"    // counters: no counter of that key
	CodeCounterExists    = "counter_exists"     // counters create: the key is taken
	CodeCounterRetired   = "counter_retired"    // counters: the counter is retired
	CodeCounterMoved     = "counter_moved"      // counters start-from: last is not expectLast (someone issued a number)
	CodeCounterLower     = "counter_lower"      // counters: a number at or below one issued (a counter never moves back)
	CodeCounterRange     = "counter_range"      // counters: the range would exclude the last or an issued number
	CodeCounterInFlight  = "counter_in_flight"  // counters retire / range: orders or receipts of its range not finished (range: left out)
	CodeCounterStore     = "counter_store"      // counters: its store cannot be read (or is missing)
	CodeIndexUnreadable  = "index_unreadable"   // counters: index entries of the kind cannot be read (acceptUnreadable: the count)
	CodeFolderUnknown    = "folder_unknown"     // documents: no folder of that key to remove
	CodeFolderMissing    = "folder_missing"     // documents: the path is not a folder on this machine
	CodeFolderForbidden  = "folder_forbidden"   // documents: a link, inside the data directory, or not a path link mode takes
	CodeFolderInFlight   = "folder_in_flight"   // documents: orders frozen for that folder are not finished
	CodeReceiptsInFlight = "receipts_in_flight" // documents: a receipt is reserved, and the receipt folder or counter would change
	// CodeFolderWritable: documents, receipts, orders — accounts besides the administrators can write to a folder the
	// connector writes documents into (plan 087 review M1); allowWritable accepts it over WinRM only.
	CodeFolderWritable = "folder_writable"
	// CodeWriterUnknown / CodeWriterTooBroad: documents' trustedWriters (review R1b) — a name the box cannot
	// resolve to an account / one that is not a single user account (a group, an alias, a well-known SID).
	CodeWriterUnknown  = "writer_unknown"
	CodeWriterTooBroad = "writer_too_broad"
	// CodeFolderReparse / CodeFolderTooLarge: folder-acl (review R1c) — a reparse point, hard link or replaced
	// entry in the folder (never followed or rewritten through) / more entries than a lock-down walks.
	CodeFolderReparse  = "folder_reparse"
	CodeFolderTooLarge = "folder_too_large"
	// CodeFolderHasExecutables: folder-acl (review T1) — the folder holds the importer's files, or files Windows runs
	// (PATHEXT's extensions, .dll, .ps1): a lock-down never tightens a folder code is run from.
	CodeFolderHasExecutables = "folder_has_executables"
	// CodeCarryOverFailed: folder-acl with carryOver (v1.0.32) — a trusted writer the check carried over could not be
	// resolved again before anything was written (its domain controller stopped answering): nothing written.
	CodeCarryOverFailed = "carry_over_failed"
	// CodeUpgradeRunning: an upgrade over the link is pending, and its setup stops the services and replaces this
	// program (review M1).
	CodeUpgradeRunning = "upgrade_running"
)
