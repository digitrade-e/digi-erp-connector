package link

import (
	"testing"
	"time"
)

// The session marker: what the link writes, the runner reads.
func TestTheSessionMarker(t *testing.T) {
	r := newRelay(t)
	_, kr := team(t, 1)
	b, rc := pairedBox(t, r, "bfl-read", kr, "127.0.0.1:1", func(c *Config) {
		forTests088(c)
		c.Build, c.LiveEvery = "v1.0.31", 50*time.Millisecond
	})
	waitFor := func(what string, ok func(SessionMarker) bool) SessionMarker {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			m, err := ReadSessionMarker(b.store.Dir)
			if err == nil && ok(m) {
				return m
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: %+v %v", what, m, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	m := waitFor("connected", func(m SessionMarker) bool { return m.Connected })
	if m.Build != "v1.0.31" || m.Epoch != rc.epoch || m.Relay != "relay-1" || m.ConnectedAt.IsZero() {
		t.Fatalf("%+v", m)
	}
	first := m.LiveAt
	waitFor("a liveness answer", func(m SessionMarker) bool { return m.LiveAt.After(first) })
	// the relay drops the connection: the link connects again at once, and the marker follows the new one
	_ = rc.sess.Close()
	waitFor("the next connection", func(m SessionMarker) bool { return m.Connected && m.Epoch != rc.epoch })
	// the service stops: the marker no longer says connected
	b.cancel()
	<-b.done
	if m, err := ReadSessionMarker(b.store.Dir); err != nil || m.Connected {
		t.Fatalf("after the stop: %+v %v", m, err)
	}
}
