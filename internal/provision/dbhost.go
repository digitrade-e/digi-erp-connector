package provision

import (
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/digitrade-e/digi-erp-connector/internal/db"
)

// checkDBHost refuses a db.host outside this machine and its LAN. Apply and
// discover log in there with the client's administrator credential, and the
// box reaches the internet: a server anywhere else would receive the password
// (in the clear, or under whatever certificate the box's encrypt settings
// accept). A name is resolved here, on the box, and every address it has must
// be loopback, private or link-local (plan 068's panel, 2026-10-20).
func checkDBHost(ctx context.Context, env Env, host string) *Refusal {
	server, _ := db.SplitInstance(host)
	if server == "" || strings.EqualFold(server, "localhost") {
		return nil
	}
	ips := []net.IP{net.ParseIP(server)}
	if ips[0] == nil {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var err error
		ips, err = env.LookupIP(ctx, server)
		if err == nil && len(ips) == 0 {
			err = errors.New("no address")
		}
		if err != nil {
			return refuse(CodeDBUnreachable, "use the SQL server's LAN address, or a name this machine resolves",
				"db.host %s does not resolve here: %v", server, err)
		}
	}
	for _, ip := range ips {
		if !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
			return refuse(CodeDBHostPublic, "use the SQL server's LAN address: the client's administrator credential goes there",
				"db.host %s is %s, outside this machine and its LAN", server, ip)
		}
	}
	return nil
}
