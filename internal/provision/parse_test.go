package provision

import (
	"encoding/binary"
	"net"
	"slices"
	"testing"
)

// A TCP table as GetExtendedTcpTable fills it: the count, then rows whose
// port sits in network byte order in the low word.
func TestParseTCPTable(t *testing.T) {
	v4 := make([]byte, 4+2*tcpRowSize4)
	binary.LittleEndian.PutUint32(v4, 2)
	row := v4[4:]
	copy(row[4:8], net.IPv4(0, 0, 0, 0).To4())
	row[8], row[9] = 0xF8, 0x0D // 63501, a WIZSOFT dynamic port
	binary.LittleEndian.PutUint32(row[20:24], 900)
	row = v4[4+tcpRowSize4:]
	copy(row[4:8], net.IPv4(127, 0, 0, 1).To4())
	row[8], row[9] = 0x1F, 0x90 // 8080
	binary.LittleEndian.PutUint32(row[20:24], 4242)

	got, err := parseTCPTable(v4, afInet)
	if err != nil {
		t.Fatal(err)
	}
	want := []Listener{{Address: "0.0.0.0", Port: 63501, PID: 900}, {Address: "127.0.0.1", Port: 8080, PID: 4242}}
	if !slices.Equal(got, want) {
		t.Errorf("IPv4: got %+v, want %+v", got, want)
	}

	v6 := make([]byte, 4+tcpRowSize6)
	binary.LittleEndian.PutUint32(v6, 1)
	row = v6[4:]
	copy(row[0:16], net.IPv6unspecified)
	row[20], row[21] = 0x1F, 0x92 // 8082
	binary.LittleEndian.PutUint32(row[52:56], 4243)
	got, err = parseTCPTable(v6, afInet6)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Listener{{Address: "::", Port: 8082, PID: 4243}}; !slices.Equal(got, want) {
		t.Errorf("IPv6: got %+v, want %+v", got, want)
	}

	if _, err := parseTCPTable(v4[:30], afInet); err == nil {
		t.Error("a table shorter than its count was read")
	}
}

func TestRuleFromPS(t *testing.T) {
	ok := psRule{Enabled: true, Direction: "Inbound", Action: "Allow", Protocol: "TCP",
		LocalPort: []string{"8082"}, RemoteAddress: []string{"198.51.100.241", "10.1.2.0/255.255.255.0"}}
	if r := ruleFromPS("r", nil); r != nil {
		t.Errorf("no rule: got %+v", r)
	}
	r := ruleFromPS("r", []psRule{ok})
	if r == nil || !r.Enabled || r.Port != 8082 || !slices.Equal(r.RemoteAddress, []string{"10.1.2.0/24", "198.51.100.241"}) {
		t.Errorf("one rule: got %+v", r)
	}
	any := ok
	any.RemoteAddress = []string{"Any"}
	if r := ruleFromPS("r", []psRule{any}); r == nil || len(r.RemoteAddress) != 0 {
		t.Errorf("any source: got %+v", r)
	}
	// Anything but one inbound TCP allow on one port matches nothing, so
	// Apply replaces it.
	outbound := ok
	outbound.Direction = "Outbound"
	ports := ok
	ports.LocalPort = []string{"8082", "8083"}
	for name, rules := range map[string][]psRule{"two rules": {ok, ok}, "outbound": {outbound}, "two ports": {ports}} {
		if r := ruleFromPS("r", rules); r == nil || r.Port != 0 {
			t.Errorf("%s: got %+v", name, r)
		}
	}
}

// The registry's TcpPort and TcpDynamicPorts: "" or "0" mean none.
func TestFirstPort(t *testing.T) {
	for in, want := range map[string]int{"63501": 63501, "1433,1434": 1433, "": 0, "0": 0, " 1433 ": 1433, "x": 0, "70000": 0} {
		if got := firstPort(in); got != want {
			t.Errorf("firstPort(%q) = %d, want %d", in, got, want)
		}
	}
}
