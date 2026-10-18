package dataacl

import (
	"strings"
	"testing"
)

// The data directory's DACL is protected (nothing inherited from
// %ProgramData%, whose ACL lets every local user read) and grants Full
// Control to exactly SYSTEM, BUILTIN\Administrators and the service account.
// A directory's ACEs are inherited by every file and folder created under it;
// a file's need no inheritance flags.
func TestSDDL(t *testing.T) {
	const user = "S-1-5-21-1004336348-1177238915-682003330-1001"
	for _, tc := range []struct {
		name      string
		sid       string
		container bool
		want      string
	}{
		{"directory, a named service account", user, true,
			"D:P(A;OICI;FA;;;S-1-5-18)(A;OICI;FA;;;S-1-5-32-544)(A;OICI;FA;;;" + user + ")(A;OICI;RC;;;S-1-3-4)"},
		{"file, a named service account", user, false,
			"D:P(A;;FA;;;S-1-5-18)(A;;FA;;;S-1-5-32-544)(A;;FA;;;" + user + ")(A;;RC;;;S-1-3-4)"},
		// The service runs as LocalSystem: its token user is SYSTEM, granted once.
		{"directory, LocalSystem", SIDLocalSystem, true,
			"D:P(A;OICI;FA;;;S-1-5-18)(A;OICI;FA;;;S-1-5-32-544)(A;OICI;RC;;;S-1-3-4)"},
		{"file, Administrators as the account", SIDAdministrators, false,
			"D:P(A;;FA;;;S-1-5-18)(A;;FA;;;S-1-5-32-544)(A;;RC;;;S-1-3-4)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SDDL(tc.sid, tc.container)
			if err != nil {
				t.Fatalf("SDDL: %v", err)
			}
			if got != tc.want {
				t.Errorf("SDDL(%q, %v)\n got %s\nwant %s", tc.sid, tc.container, got, tc.want)
			}
		})
	}
}

// Nothing but the three principals with Full Control, plus OWNER RIGHTS with
// READ_CONTROL alone: no Users, Everyone, Authenticated Users or CREATOR
// OWNER, no deny ACEs.
func TestSDDL_GrantsNobodyElse(t *testing.T) {
	got, err := SDDL("S-1-5-21-1-2-3-1001", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "D:P(") {
		t.Errorf("%s: not a protected DACL", got)
	}
	if n := strings.Count(got, "(A;OICI;FA;;;"); n != 3 || strings.Count(got, "(") != 4 {
		t.Errorf("%s: want exactly three full-control allow ACEs and one more", got)
	}
	for _, other := range []string{"BU", "WD", "AU", "CO", "S-1-1-0", "S-1-5-32-545", "S-1-5-11", "(D;"} {
		if strings.Contains(got, ";"+other+")") || strings.Contains(got, other+";") {
			t.Errorf("%s grants or names %s", got, other)
		}
	}
}

// Only a SID string goes into the SDDL: a token user that did not render as
// one must never be spliced in (an injected ")(A;;FA;;;WD" would grant
// Everyone).
func TestSDDL_RefusesAnythingButASID(t *testing.T) {
	for _, bad := range []string{
		"", " ", "BU", "WD", "s-1-5-18", "S-1-5", "S-1-", "S-1-5-21-1)(A;;FA;;;WD",
		"S-1-5-18 ", " S-1-5-18", "S-1-5-x", "S-1-5-21-1-2-3-1001\n",
	} {
		if got, err := SDDL(bad, true); err == nil {
			t.Errorf("SDDL(%q) = %q; want refused", bad, got)
		}
	}
}

// OWNER RIGHTS (S-1-3-4) replaces the owner's implicit READ_CONTROL and
// WRITE_DAC with what its ACE grants. The ACE grants READ_CONTROL alone, and
// is inherited like the rest: whoever owns the directory or a file in it —
// often the administrator or process that created it — can read its DACL but
// can no longer rewrite it without being SYSTEM, an Administrator or the
// service account.
func TestSDDL_OwnerRightsIsReadControlOnly(t *testing.T) {
	for _, container := range []bool{true, false} {
		got, err := SDDL("S-1-5-21-1-2-3-1001", container)
		if err != nil {
			t.Fatal(err)
		}
		want := "(A;;RC;;;" + SIDOwnerRights + ")"
		if container {
			want = "(A;OICI;RC;;;" + SIDOwnerRights + ")"
		}
		if strings.Count(got, SIDOwnerRights) != 1 || !strings.Contains(got, want) {
			t.Errorf("SDDL(container=%v) = %s; want exactly one %s", container, got, want)
		}
	}
	if SIDOwnerRights != "S-1-3-4" {
		t.Errorf("SIDOwnerRights = %s", SIDOwnerRights)
	}
}
