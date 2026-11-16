package provision

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/digitrade-e/digi-erp-connector/internal/config"
)

func linkBox(t *testing.T, sys *fakeSys) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(sys.DataDir(), "link.yaml"), []byte("box: bfl-read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func check(res *VerifyResult, name string) (Check, bool) {
	for _, c := range res.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// Plan 086 C10: verify's listen check takes a loopback apiListen behind the
// link (where it is the rule), and still fails it elsewhere.
func TestVerify_LoopbackListenBehindTheLink(t *testing.T) {
	sys, fdb, _ := handInstallBox(t) // listens on 127.0.0.1:8080
	env := testEnv(sys, fdb)
	res, err := Verify(context.Background(), env, VerifyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := check(res, "listen"); c.OK {
		t.Error("a loopback listen passed without the link")
	}
	linkBox(t, sys)
	res, err = Verify(context.Background(), env, VerifyInput{})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := check(res, "listen"); !c.OK {
		t.Errorf("a loopback listen failed behind the link: %+v", c)
	}
}

// A database reached by port belongs to the instance with that port: BFL's
// 127.0.0.1:63501 is MSSQL$WIZSOFT2017, not MSSQLSERVER (plan 083 §19).
func TestSQLDependencyFor(t *testing.T) {
	instances := []SQLInstance{
		{Name: "MSSQLSERVER", Service: "MSSQLSERVER", StaticPort: 1433},
		{Name: "WIZSOFT2017", Service: "MSSQL$WIZSOFT2017", DynamicPort: 63501},
		{Name: "SQLEXPRESS", Service: "MSSQL$SQLEXPRESS", ListeningPorts: []int{50111}},
	}
	tests := []struct {
		host string
		port int
		want string
	}{
		{"127.0.0.1", 63501, "MSSQL$WIZSOFT2017"},
		{"localhost", 50111, "MSSQL$SQLEXPRESS"},
		{"127.0.0.1", 1433, "MSSQLSERVER"},
		{"127.0.0.1", 9999, "MSSQLSERVER"}, // a port no instance has: the default instance's, as before
		{`127.0.0.1\wizsoft2017`, 0, "MSSQL$WIZSOFT2017"},
		{"10.0.0.9", 63501, ""}, // elsewhere: nothing to depend on
		{"", 0, ""},             // no database
	}
	for _, tc := range tests {
		cfg := config.Config{DB: config.DBConfig{Host: tc.host, Port: tc.port}}
		if got := sqlDependencyFor(cfg, instances); got != tc.want {
			t.Errorf("%s:%d = %q, want %q", tc.host, tc.port, got, tc.want)
		}
	}
}

// discover's suggestion on a linked box: loopback, and the named instance's
// service for a database reached by port.
func TestDiscover_SuggestionBehindTheLink(t *testing.T) {
	sys, fdb, _ := handInstallBox(t) // 127.0.0.1:63501, listening on 127.0.0.1:8080
	sys.sqlBrowser.State = "stopped" // so the suggestion keeps the port
	sys.instances = []SQLInstance{
		{Name: "MSSQLSERVER", Service: "MSSQLSERVER", StaticPort: 1433, TCPEnabled: true},
		{Name: "WIZSOFT2017", Service: "MSSQL$WIZSOFT2017", DynamicPort: 63501, TCPEnabled: true},
	}
	env := testEnv(sys, fdb)
	f, err := Discover(context.Background(), env, DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Suggested.APIListen != "0.0.0.0:8080" {
		t.Errorf("without the link apiListen = %s", f.Suggested.APIListen)
	}
	if !slices.Equal(f.Suggested.DependOn, []string{"MSSQL$WIZSOFT2017"}) {
		t.Errorf("dependOn = %v, want MSSQL$WIZSOFT2017", f.Suggested.DependOn)
	}
	linkBox(t, sys)
	f, err = Discover(context.Background(), env, DiscoverInput{})
	if err != nil {
		t.Fatal(err)
	}
	if f.Suggested.APIListen != "127.0.0.1:8080" {
		t.Errorf("behind the link apiListen = %s, want 127.0.0.1:8080", f.Suggested.APIListen)
	}
	// verify names the same dependency
	sys.svc.Dependencies = []string{"MSSQL$WIZSOFT2017"}
	res, _ := Verify(context.Background(), env, VerifyInput{})
	if c, ok := check(res, "dependency"); !ok || !c.OK || !strings.Contains(c.Detail, "MSSQL$WIZSOFT2017") {
		t.Errorf("verify's dependency check: %+v", c)
	}
}

func TestDiscover_NoConfigBehindTheLinkSuggestsLoopback(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	_ = os.Remove(filepath.Join(sys.dir, configFile))
	linkBox(t, sys)
	f, err := Discover(context.Background(), testEnv(sys, fdb), DiscoverInput{})
	if err != nil || f.Suggested.APIListen != "127.0.0.1:8082" {
		t.Errorf("apiListen %s, %v", f.Suggested.APIListen, err)
	}
}

// discover and verify take an empty stdin as {} (over the link a command
// always has a stdin); anything else must still decode strictly.
func TestRun_EmptyStdinIsAnEmptyObject(t *testing.T) {
	sys, fdb, _ := handInstallBox(t)
	env := testEnv(sys, fdb)
	for _, cmd := range []string{"discover", "verify"} {
		for _, stdin := range []string{"", " \r\n", "{}"} {
			out, code := Run(context.Background(), env, []string{cmd, "--spec", "-"}, strings.NewReader(stdin))
			if strings.Contains(string(out), CodeSpecInvalid) {
				t.Errorf("%s with stdin %q: code %d, %s", cmd, stdin, code, out)
			}
		}
		out, code := Run(context.Background(), env, []string{cmd, "--spec", "-"}, strings.NewReader(`{"nope":1}`))
		if code != ExitRefused || !strings.Contains(string(out), CodeSpecInvalid) {
			t.Errorf("%s with an unknown field: code %d", cmd, code)
		}
		out, code = Run(context.Background(), env, []string{cmd, "--spec", "-"}, strings.NewReader(`{} {}`))
		if code != ExitRefused || !strings.Contains(string(out), CodeSpecInvalid) {
			t.Errorf("%s with trailing data: code %d", cmd, code)
		}
	}
}
