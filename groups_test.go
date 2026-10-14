package main

import (
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

func groupJID(user string) types.JID { return types.NewJID(user, types.GroupServer) }

func group(user, name string, parent types.JID) *types.GroupInfo {
	g := &types.GroupInfo{JID: groupJID(user)}
	g.Name = name
	g.LinkedParentJID = parent
	return g
}

func community(user, name string) *types.GroupInfo {
	g := group(user, name, types.EmptyJID)
	g.IsParent = true
	return g
}

func TestSelectGroups(t *testing.T) {
	seth := community("100", "Seth")
	other := community("200", "Other School")
	announcements := group("101", "Seth", seth.JID) // the announcement group is named after the community
	announcements.IsDefaultSubGroup = true
	known := []*types.GroupInfo{
		seth, other, announcements,
		group("102", "Preschool", seth.JID),
		group("103", "Cleanup Duties/Campus Security", seth.JID),
		group("104", "Book Club", seth.JID),       // in the community, not listed
		group("201", "Preschool", other.JID),      // same name, other community
		group("300", "Preschool", types.EmptyJID), // same name, no community
	}
	wanted := []string{"Announcements", "preschool", "Cleanup  Duties/Campus Security", "Yearbook Committee", "Preschool"}

	sel, err := selectGroups(known, "seth", wanted)
	if err != nil {
		t.Fatal(err)
	}
	if sel.CommunityJID != seth.JID || sel.CommunityName != "Seth" {
		t.Errorf("community = %s %q, want %s Seth", sel.CommunityJID, sel.CommunityName, seth.JID)
	}
	wantGroups := []selectedGroup{
		{JID: groupJID("101"), Name: "Announcements", IsAnnouncement: true},
		{JID: groupJID("102"), Name: "Preschool"},
		{JID: groupJID("103"), Name: "Cleanup Duties/Campus Security"},
	}
	if !reflect.DeepEqual(sel.Groups, wantGroups) {
		t.Errorf("groups = %+v, want %+v", sel.Groups, wantGroups)
	}
	if !reflect.DeepEqual(sel.Missing, []string{"Yearbook Committee"}) {
		t.Errorf("missing = %q, want [Yearbook Committee]", sel.Missing)
	}
	if !reflect.DeepEqual(sel.Unlisted, []string{"Book Club"}) {
		t.Errorf("unlisted = %q, want [Book Club]", sel.Unlisted)
	}

	// The community can also be given by JID, even if its own info isn't known.
	sel, err = selectGroups(known[2:], seth.JID.String(), []string{"Preschool"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sel.Groups) != 1 || sel.Groups[0].JID != groupJID("102") {
		t.Errorf("by JID: groups = %+v, want only 102", sel.Groups)
	}

	if _, err := selectGroups(known, "Nowhere", wanted); err == nil || !strings.Contains(err.Error(), `"Other School", "Seth"`) {
		t.Errorf("unknown community: err = %v, want one listing the communities", err)
	}
	twin := community("400", "Seth")
	if _, err := selectGroups(append(known, twin), "Seth", wanted); err == nil || !strings.Contains(err.Error(), "several communities") {
		t.Errorf("two communities named Seth: err = %v", err)
	}
}
