package linkproto

// Refusal is every check's error: a stable code the relay, the box and
// digi-mcp log and show, never the input that caused it.
type Refusal struct {
	Code string
}

func (r *Refusal) Error() string { return "linkproto: " + r.Code }

// The codes. They are part of the protocol: a caller may switch on them.
const (
	CodeBadKey         = "bad_key"
	CodeBadSignature   = "signature_refused"
	CodeMalformed      = "malformed"
	CodeVersion        = "version_unsupported"
	CodeKeyUnknown     = "key_unknown"
	CodeKeyRevoked     = "key_revoked"
	CodeExpired        = "expired"
	CodeFromFuture     = "from_future"
	CodeLifetime       = "lifetime_too_long"
	CodeWrongBox       = "wrong_box"
	CodeWrongOp        = "wrong_op"
	CodeBodyMismatch   = "body_mismatch"
	CodeWrongNonce     = "wrong_nonce"
	CodeFrameTooLarge  = "frame_too_large"
	CodeBadPairCode    = "bad_pair_code"
	CodeBoxUnknown     = "box_unknown"
	CodeBoxKeyChanged  = "box_key_changed"
	CodePending        = "pending_confirmation"
	CodePairExpired    = "pair_code_expired"
	CodeReplay         = "replayed_nonce"
	CodeCommandUnknown = "command_unknown"

	// What a box answers an installer's command (plan 090 D3), after its certificate and signature verified.
	CodeInstallerNotBound    = "installer_not_bound"     // the box was paired by another installer, or by the team
	CodeInstallerNotAllowed  = "installer_not_allowed"   // a command an installer may not send
	CodeInstallerInput       = "installer_input_refused" // an installer's command carries an input
	CodeInstallerDC          = "installer_dc"            // the box is a domain controller: the team sets it up
	CodeInstallerRoleUnknown = "installer_role_unknown"  // the box could not read its domain role: treated as a DC
)

func refuse(code string) error { return &Refusal{Code: code} }
