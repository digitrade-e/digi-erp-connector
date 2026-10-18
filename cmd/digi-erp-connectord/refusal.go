package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// refusalRepeat is how often the same refusal reason is written to the Event
// Log again: the service manager restarts a refused service every 30 min.
const refusalRepeat = 24 * time.Hour

// refusalDue decides whether a refusal is written to the Event Log: when its
// reason differs from the last one written, or that was refusalRepeat ago.
// state is "<reason hash> <unix time>" of the last one written (kept outside
// the data directory, see refusal_windows.go); it returns the state to keep.
func refusalDue(state, reason string, now time.Time) (bool, string) {
	sum := sha256.Sum256([]byte(reason))
	hash := hex.EncodeToString(sum[:8])
	next := hash + " " + strconv.FormatInt(now.Unix(), 10)
	f := strings.Fields(state)
	if len(f) != 2 || f[0] != hash {
		return true, next
	}
	at, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil || now.Sub(time.Unix(at, 0)) >= refusalRepeat {
		return true, next
	}
	return false, state
}

// refusalText is the event's text.
func refusalText(lines []string, err error) string {
	text := fmt.Sprintf("digi-erp-connectord refused to start: %v\r\n\r\nSee \"The service will not start\" in docs/troubleshooting.md. "+
		"The same reason is written again at most once a day; the service is restarted every 30 min.\r\n\r\n%s",
		err, strings.Join(lines, "\r\n"))
	if len(text) > 30000 {
		text = text[:30000]
	}
	return text
}
