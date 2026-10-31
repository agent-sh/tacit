package record

import (
	"testing"

	"github.com/agent-sh/tacit/client/internal/check"
)

func orgRec(name string) *Record {
	return &Record{Folder: "orgs/" + name, Kind: KindOrg, Name: name, Description: "the " + name + " organization", Status: StatusActive,
		Was:      []Entry{{Status: StatusActive, Date: today, By: ByOpus, Why: "code: the forge says " + name + " is an organization"}},
		Evidence: []Evidence{{Gathered: today, Checks: []check.Result{{Kind: "account", Verdict: "holds", Value: "Organization", Inputs: map[string]string{"name": name}}}}},
		Created:  today, CreatedBy: ByOpus}
}

// An org record rests on the forge's check, not on a session; it never
// folds into anything; only the project folder named for it folds into it.
func TestOrgRecords(t *testing.T) {
	o := orgRec("agent-sh")
	expect(t, Validate(nil, o, ByOpus, env()), "")
	noCheck := orgRec("agent-sh")
	noCheck.Evidence = nil
	expect(t, Validate(nil, noCheck, ByOpus, env()), "cites no session")
	if FolderKinds("orgs/agent-sh")[0] != KindOrg || IsFolder("orgs/a/b") {
		t.Fatal("orgs/<name> holds an org")
	}

	tc := repoRec("tacit")
	into := orgRec("agent-sh")
	into.SuppressedBy = "projects/tacit"
	expect(t, Validate(nil, into, ByOpus, env(tc)), "an org folder is never folded")

	// projects/agent-sh folds into orgs/agent-sh by code, with no session.
	p := &Record{Folder: "projects/agent-sh", Kind: KindFolder, Name: "agent-sh", Description: "named for the org", Status: StatusDead,
		Was:     []Entry{{Status: StatusDead, Date: today, By: ByOpus, Why: "not a project: agent-sh is an organization"}},
		Created: today, CreatedBy: ByOpus, SuppressedBy: "orgs/agent-sh",
		Evidence: []Evidence{{Gathered: today, FactSessions: 3}}}
	expect(t, Validate(nil, p, ByOpus, env(o)), "")
	// Another project does not fold into an org.
	q := repoRec("gizmo")
	q.SuppressedBy = "orgs/agent-sh"
	expect(t, Validate(nil, q, ByOpus, env(o)), "names a folder of another kind")
	if !OrgFolds(p, o) || OrgFolds(q, o) {
		t.Fatal("OrgFolds")
	}
}

// A project's org and forge name are well formed and only a project's; the
// display name is the folder's with the current forge name beside it when
// the two differ.
func TestOrgAndForgeFields(t *testing.T) {
	r := repoRec("storefront")
	r.Forge, r.Org = "github.com/acme/shop-v2", ""
	expect(t, Validate(nil, r, ByOpus, env()), "")
	if d := r.Display(); d != "storefront (GitHub: shop-v2)" {
		t.Fatalf("display %q", d)
	}
	r.Forge = "github.com/acme/storefront"
	if d := r.Display(); d != "storefront" {
		t.Fatalf("display %q", d)
	}
	r.Forge, r.Org = "acme/shop-v2", "Agent SH"
	ps := Validate(nil, r, ByOpus, env())
	expect(t, ps, "forge: only a project has a forge name")
	expect(t, ps, "org: only a project has an org")
	o := orgRec("agent-sh")
	o.Org, o.Forge = "agent-sh", "github.com/agent-sh/x"
	expect(t, Validate(nil, o, ByOpus, env()), "org: only a project has an org")
	b, err := (func() ([]byte, error) {
		x := repoRec("tacit")
		x.Org, x.Forge = "agent-sh", "github.com/agent-sh/tacit"
		return x.Encode()
	})()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse("projects/tacit", b)
	if err != nil || back.Org != "agent-sh" || back.Forge != "github.com/agent-sh/tacit" {
		t.Fatalf("round trip %+v %v", back, err)
	}
}
