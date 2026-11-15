package link

import "testing"

// The compiled-in keyring is what every box of this release trusts: exactly the team key digi-mcp signs with, at an
// epoch no lower than CompiledMinEpoch (a key below it would be refused on every box, silently).
func TestTheCompiledInTeamKey(t *testing.T) {
	kr, err := TeamKeyring()
	if err != nil {
		t.Fatal(err)
	}
	if len(kr) != 1 || kr[0].ID != "team-1" || kr[0].Epoch != 1 || len(kr[0].Pub) != 32 {
		t.Fatalf("keyring: %+v", kr)
	}
	for _, k := range kr {
		if k.Epoch < CompiledMinEpoch {
			t.Errorf("%s epoch %d is below CompiledMinEpoch %d", k.ID, k.Epoch, CompiledMinEpoch)
		}
	}
}
