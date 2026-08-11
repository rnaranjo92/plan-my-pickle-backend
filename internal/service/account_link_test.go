package service

import "testing"

// accountForContact decides whether an organizer-typed player gets tied to a
// real account. It must key off EMAIL/PHONE only: organizers type nicknames and
// misspellings (a live roster row read "Kay" for an account named "Kim
// Naranji"), and the old phone branch required the name to match too, so those
// people silently stayed unlinked and never saw the league in their app.

func TestAccountForContact_LinksDespiteWrongName(t *testing.T) {
	f := newFake().seedRPC("pmp_account_by_contact", `"acct-1"`)
	s := newFakeSvc(t, f)

	// Same contact info, wildly different names — every one must still link.
	for _, name := range []string{"Kay", "Kim Naranji", "", "k a y", "Kaye N."} {
		if got := s.accountForContact("kay@example.com", "", name); got != "acct-1" {
			t.Fatalf("email match with name %q = %q, want acct-1 "+
				"(the name must not affect linking)", name, got)
		}
	}
}

func TestAccountForContact_LinksByPhoneAloneRegardlessOfFormat(t *testing.T) {
	f := newFake().seedRPC("pmp_account_by_contact", `"acct-1"`)
	s := newFakeSvc(t, f)

	// No name at all, and formatting that raw string equality never matched.
	for _, phone := range []string{"(619) 555-0100", "619-555-0100", "6195550100"} {
		if got := s.accountForContact("", phone, ""); got != "acct-1" {
			t.Fatalf("phone %q = %q, want acct-1 (phone alone must link)", phone, got)
		}
	}
}

// No contact info means nothing to match on. Linking on a bare name would
// attach strangers to each other's accounts, so this must stay unlinked — it is
// exactly the case that produced 10 disconnected roster rows in production.
func TestAccountForContact_NameOnlyNeverLinks(t *testing.T) {
	f := newFake().seedRPC("pmp_account_by_contact", `"acct-1"`)
	s := newFakeSvc(t, f)

	if got := s.accountForContact("", "", "Kay Naranjo"); got != "" {
		t.Fatalf("name-only lookup linked to %q, want no match", got)
	}
}

// A null/blank RPC answer (no account, or the migration hasn't run) must not be
// mistaken for a user id — that would link a registration to the string "null".
func TestAccountForContact_NoMatchIsNotAnID(t *testing.T) {
	for _, reply := range []string{`null`, `""`, ``} {
		f := newFake().seedRPC("pmp_account_by_contact", reply)
		s := newFakeSvc(t, f)
		if got := s.accountForContact("nobody@example.com", "", ""); got != "" {
			t.Fatalf("RPC reply %q produced id %q, want no match", reply, got)
		}
	}
}
