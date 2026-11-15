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
)

func refuse(code string) error { return &Refusal{Code: code} }
