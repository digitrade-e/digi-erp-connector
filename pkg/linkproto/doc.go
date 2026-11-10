// Package linkproto is the wire protocol between a connector box's link service
// (digi-erp-connector-link), the relay (github.com/digitrade-e/connector-link)
// and digi-mcp — Digitrade plan 083.
//
// It is public on purpose: the relay and digi-mcp import it, and its security
// rests on keys, never on the protocol being unknown.
//
// # The connection
//
// The box holds one WebSocket to the relay's /link. The relay sends a
// Challenge; the box answers with a Hello signed by its Ed25519 key (and, at
// pairing, the code a human typed); the relay answers with a Welcome. Then the
// WebSocket carries one yamux session. The relay opens streams; the box opens
// exactly one, its control stream. Every stream starts with one type byte
// (StreamData, StreamCommand, StreamControl); command and control streams then
// carry length-prefixed frames (WriteFrame, ReadFrame).
//
// # Who can make a box do what
//
// The relay is a forwarder, not an authority. A command reaches a box only as
// an Envelope signed by a team key the box's release compiles in (Keyring); the
// box checks the signature, its own box id, the expiry and the nonce, and signs
// its Answer with its own key, which digi-mcp pinned at pairing. Secrets for
// the box are sealed to its X25519 key (Seal). Requests to the relay's own
// admin operations (pairing, the box list) are signed the same way
// (AdminRequest), so the relay needs no identity provider of its own.
package linkproto

// Version is the protocol version every message carries in "v".
const Version = 1
