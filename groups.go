package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/tianqitong/seth-ingestion/db"
	"github.com/tianqitong/seth-ingestion/internal/grouplist"
)

// announcementsName is how WhatsApp shows a community's announcement group,
// whatever the group's own name is. Use it in the groups file to monitor it.
const announcementsName = "Announcements"

// selectedGroup is a group of the community that the groups file lists.
type selectedGroup struct {
	JID            types.JID
	Name           string // as WhatsApp shows it; announcementsName for the announcement group
	IsAnnouncement bool
}

type groupSelection struct {
	CommunityJID  types.JID
	CommunityName string
	Groups        []selectedGroup // in groups file order
	Missing       []string        // listed, but not a group of the community that you're in
	Unlisted      []string        // groups of the community that you're in but that aren't listed
}

// selectGroups finds the community (by name, or by JID if community is one)
// among known, and picks the community's groups that wanted names. known holds
// the groups you're in, plus any communities they belong to.
func selectGroups(known []*types.GroupInfo, community string, wanted []string) (groupSelection, error) {
	var sel groupSelection
	if jid, err := types.ParseJID(community); err == nil && jid.Server == types.GroupServer {
		sel.CommunityJID = jid
		for _, g := range known {
			if g.JID == jid {
				sel.CommunityName = g.Name
			}
		}
	} else {
		var matches []*types.GroupInfo
		var names []string
		for _, g := range known {
			if !g.IsParent {
				continue
			}
			names = append(names, g.Name)
			if grouplist.Key(g.Name) == grouplist.Key(community) {
				matches = append(matches, g)
			}
		}
		switch len(matches) {
		case 0:
			sort.Strings(names)
			return sel, fmt.Errorf("you're not in a community named %q. Communities found: %s", community, listOrNone(names))
		case 1:
			sel.CommunityJID, sel.CommunityName = matches[0].JID, matches[0].Name
		default:
			var jids []string
			for _, g := range matches {
				jids = append(jids, g.JID.String())
			}
			return sel, fmt.Errorf("several communities are named %q (%s); set -community to the JID of the one to use", community, strings.Join(jids, ", "))
		}
	}

	byName := map[string][]selectedGroup{}
	for _, g := range known {
		if g.IsParent || g.LinkedParentJID != sel.CommunityJID {
			continue
		}
		sg := selectedGroup{JID: g.JID, Name: g.Name, IsAnnouncement: g.IsDefaultSubGroup}
		if sg.IsAnnouncement {
			sg.Name = announcementsName
		}
		byName[grouplist.Key(sg.Name)] = append(byName[grouplist.Key(sg.Name)], sg)
	}
	matched := map[string]bool{}
	for _, name := range wanted {
		key := grouplist.Key(name)
		if matched[key] {
			continue
		}
		matched[key] = true
		if len(byName[key]) == 0 {
			sel.Missing = append(sel.Missing, name)
			continue
		}
		sel.Groups = append(sel.Groups, byName[key]...)
		delete(byName, key)
	}
	for _, groups := range byName {
		for _, g := range groups {
			sel.Unlisted = append(sel.Unlisted, g.Name)
		}
	}
	sort.Strings(sel.Unlisted)
	return sel, nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return `"` + strings.Join(names, `", "`) + `"`
}

// findGroups looks up the groups to monitor on WhatsApp, records them in the
// store, and returns them by JID. It logs listed groups that weren't found and
// community groups that aren't listed.
func findGroups(ctx context.Context, client *whatsmeow.Client, store *db.Store, community string, wanted []string, log waLog.Logger) (map[types.JID]*db.Group, error) {
	known, err := client.GetJoinedGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list your groups: %w", err)
	}
	// A community you belong to is normally listed with your groups. If one
	// isn't, look it up so that it can be found by name.
	have := map[types.JID]bool{}
	for _, g := range known {
		have[g.JID] = true
	}
	for _, g := range known {
		parent := g.LinkedParentJID
		if parent.IsEmpty() || have[parent] {
			continue
		}
		have[parent] = true
		info, err := client.GetGroupInfo(ctx, parent)
		if err != nil {
			log.Debugf("Couldn't look up community %s: %v", parent, err)
			continue
		}
		known = append(known, info)
	}

	sel, err := selectGroups(known, community, wanted)
	if err != nil {
		return nil, err
	}
	communityLabel := sel.CommunityName
	if communityLabel == "" {
		communityLabel = sel.CommunityJID.String()
	}
	for _, name := range sel.Missing {
		log.Warnf("Group %q isn't one of your groups in the %s community, so it won't be recorded. Check its spelling in the groups file, or whether it was renamed", name, communityLabel)
	}
	if len(sel.Unlisted) > 0 {
		log.Infof("Not recording these groups in the %s community, because the groups file doesn't list them: %s", communityLabel, listOrNone(sel.Unlisted))
	}
	if len(sel.Groups) == 0 {
		return nil, fmt.Errorf("none of the listed groups were found in the %s community", communityLabel)
	}

	groups := make(map[types.JID]*db.Group, len(sel.Groups))
	for _, sg := range sel.Groups {
		g, err := store.SaveGroup(ctx, db.Group{
			JID:            sg.JID.String(),
			Name:           sg.Name,
			CommunityJID:   sel.CommunityJID.String(),
			CommunityName:  sel.CommunityName,
			IsAnnouncement: sg.IsAnnouncement,
			MediaFolder:    grouplist.FolderName(sg.Name, sg.JID.User),
		})
		if err != nil {
			return nil, fmt.Errorf("record group %q: %w", sg.Name, err)
		}
		groups[sg.JID] = &g
	}
	return groups, nil
}
