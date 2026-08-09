package dto

// TokenRequest is the body of POST /auth/token.
type TokenRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// TokenResponse is what POST /auth/token returns on success.
//
// The field names are a contract with callers written against the Node service
// this connector replaced: at least one reads `access_token` and nothing else,
// and stores whatever it finds — so the key must be present on every 200, or
// that caller sends an empty credential on its next request and loops on 401.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

// PingResponse is {"ok":true,"ts":<epoch millis>} from GET /api/ping.
//
// `ts` is milliseconds because that is what the original emitted (JS Date.now).
type PingResponse struct {
	OK bool  `json:"ok"`
	TS int64 `json:"ts"`
}
