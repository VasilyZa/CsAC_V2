package legacy

import (
	"math"
	"sort"
	"strings"
	"time"
)

func (b *builder) mapGroups(rooms []sourceRoom, members []sourceMember, admins []sourceAdmin) {
	const roomTable = "chat_room"
	const memberTable = "chat_group_user"
	codeCount := make(map[string]int)
	for _, room := range rooms {
		b.maxRoom = max(b.maxRoom, room.ID)
		if room.Search != 0 && room.Disbanded == 0 {
			if code := legacyJoinCode(room); code != "" {
				codeCount[code]++
			}
		}
	}
	for _, room := range rooms {
		if room.ID <= 0 || b.group(room.ID) != nil {
			b.block(roomTable, "invalid_group_identity")
			continue
		}
		if !b.userExists(room.Owner) {
			b.note(roomTable, "orphan_room_owner")
			continue
		}
		c := conversationRow{ID: room.ID, Owner: room.Owner, Kind: "group", Title: b.keepText(roomTable, room.Title),
			Intro: b.keepText(roomTable, room.Intro), Notice: b.keepText(roomTable, room.Notice), Visibility: "private",
			Policy: "invite", AllowInvite: room.AllowInvite != 0, SingleUse: true, Discoverable: room.Listed != 0,
			TransferCooldownUntil: optionalEpoch(room.TransferCooldown)}
		if room.Search != 0 {
			c.Visibility = "public"
		}
		if room.Search != 0 {
			switch room.JoinType {
			case 1:
				c.Policy = "open"
			case 2, 3:
				if code := legacyJoinCode(room); code != "" && codeCount[code] == 1 {
					c.Policy, c.Code, c.SingleUse = "code", code, room.JoinType == 2
					if room.JoinType == 3 {
						c.FixedCode = code
					}
				} else {
					b.note(roomTable, "invalid_or_duplicate_code_uses_invite")
				}
			case 4:
				c.Policy, c.Question = "approval", b.keepText(roomTable, room.Question)
			default:
				b.note(roomTable, "unknown_join_policy_uses_invite")
			}
		}
		switch {
		case room.Disbanded != 0:
			c.Archived = optionalEpoch(room.DisbandTime)
			if c.Archived == nil {
				c.Archived = &b.report.Options.AsOf
				b.note(roomTable, "disband_time_unavailable")
			}
		case room.BanUntil > b.report.Options.AsOf.Unix():
			c.SiteBanUntil = optionalEpoch(room.BanUntil)
			c.SiteBanReason = strings.TrimSpace(b.keepText(roomTable, room.BanReason))
			if c.SiteBanReason == "" {
				c.SiteBanReason = "Legacy site suspension"
				b.note(roomTable, "missing_group_ban_reason")
			}
		}
		if c.Archived != nil {
			c.Code, c.FixedCode, c.Policy = "", "", "invite"
		}
		if room.Avatar != "" {
			c.Avatar = b.attach(mediaRef{Table: roomTable, ID: room.ID, Owner: room.Owner, Path: room.Avatar,
				Scope: "group-avatar", Room: room.ID, Created: b.report.Options.AsOf})
		}
		b.rooms[c.ID] = len(b.conversations)
		b.conversations = append(b.conversations, c)
	}
	adminSet := make(map[[2]int64]bool)
	for _, admin := range admins {
		key := memberKey(admin.Room, admin.User)
		if b.group(admin.Room) == nil || !b.userExists(admin.User) || adminSet[key] {
			b.note("chat_group_admin", "invalid_group_admin")
			continue
		}
		adminSet[key] = true
	}
	for _, raw := range members {
		group := b.group(raw.Room)
		if group == nil || !b.userExists(raw.User) {
			b.note(memberTable, "orphan_membership")
			continue
		}
		key := memberKey(raw.Room, raw.User)
		if _, exists := b.memberKey[key]; exists {
			b.block(memberTable, "duplicate_membership")
			continue
		}
		level := int16(1)
		if raw.Level >= 1 && raw.Level <= 100 {
			level = int16(raw.Level)
		} else {
			b.note(memberTable, "unsupported_member_level")
		}
		role := "member"
		if group.Owner == raw.User {
			role = "owner"
		} else if adminSet[key] {
			role = "admin"
		}
		delete(adminSet, key)
		joined := b.wallTime(memberTable, raw.Joined, b.wall)
		group.Created = earlier(group.Created, joined)
		title := b.keepText(memberTable, raw.Title)
		if raw.TitleCustom == 0 {
			switch title {
			case "青铜", "白银", "黄金", "铂金", "王者":
				title = ""
			}
		}
		b.addMember(memberRow{Conversation: raw.Room, User: raw.User, Role: role, Title: title,
			Level: level, LevelCustom: raw.LevelCustom != 0, Joined: joined, Mute: optionalEpoch(raw.Mute), Read: raw.Read})
	}
	for _, group := range b.conversations {
		if _, hasOwner := b.memberKey[memberKey(group.ID, group.Owner)]; !hasOwner {
			// Authority is explicit in chat_room.owner_uid. An absent member in
			// a dissolved room is expected; rebuilding it would grant access.
			if group.Archived == nil {
				b.note(roomTable, "owner_missing_membership")
				b.addMember(memberRow{Conversation: group.ID, User: group.Owner, Role: "owner", Level: 1, Joined: b.report.Options.AsOf})
			}
		}
	}
	b.reportN("chat_group_admin", "admin_without_membership", len(adminSet))
}

func legacyJoinCode(room sourceRoom) string {
	var code string
	switch room.JoinType {
	case 2:
		code = room.Code
	case 3:
		code = room.FixedCode
	}
	for _, r := range code {
		if !(r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z') {
			return ""
		}
	}
	return code
}

func (b *builder) mapFriends(friends []sourceFriend, requests []sourceFriendRequest) {
	// A correct v1 row is canonical (v1 lookups use min/max). Detect a
	// reversed duplicate before selecting any relationship or block authority.
	for _, r := range friends {
		if r.First == r.Second || !b.userExists(r.First) || !b.userExists(r.Second) {
			b.note("friend_relation", "orphan_or_self_friendship")
		}
	}
	valid := func(r sourceFriend) bool {
		return r.First != r.Second && b.userExists(r.First) && b.userExists(r.Second)
	}
	counts := make(map[pair]int)
	for _, r := range friends {
		if valid(r) {
			counts[orderedPair(r.First, r.Second)]++
		}
	}
	blocked := make(map[pair]bool)
	active := make(map[pair]bool)
	for _, r := range friends {
		if !valid(r) {
			continue
		}
		key := orderedPair(r.First, r.Second)
		if counts[key] != 1 {
			b.block("friend_relation", "ambiguous_duplicate_friendship")
			continue
		}
		switch r.Status {
		case 0, 3:
		case 1:
			active[key] = true
			b.friends = append(b.friends, friendshipRow{Pair: key, Created: b.wallTime("friend_relation", r.Created, b.wall)})
		case 2:
			b.mapFriendDeletion(r, key)
		case 4:
			b.friends = append(b.friends, friendshipRow{Pair: key, Created: b.wallTime("friend_relation", r.Created, b.wall)})
			blocked[key] = true
			if r.Actor != r.First && r.Actor != r.Second {
				// The v1 client cannot establish the actor. Keep the block in
				// both directions so cutover cannot reopen communication.
				b.note("friend_relation", "unknown_block_actor_blocks_both")
			}
		default:
			b.note("friend_relation", "unknown_friendship_status")
		}
		firstBlocked := r.Status == 4 && r.Actor != r.Second
		secondBlocked := r.Status == 4 && r.Actor != r.First
		if r.Remark1 != "" || firstBlocked {
			b.preferences = append(b.preferences, preferenceRow{Owner: r.First, Other: r.Second, Remark: b.keepText("friend_relation", r.Remark1), Blocked: firstBlocked})
		}
		if r.Remark2 != "" || secondBlocked {
			b.preferences = append(b.preferences, preferenceRow{Owner: r.Second, Other: r.First, Remark: b.keepText("friend_relation", r.Remark2), Blocked: secondBlocked})
		}
	}
	const requestTable = "friend_request"
	pending := make(map[pair]bool)
	// Only the newest pending request is actionable for a pair.
	for i := len(requests) - 1; i >= 0; i-- {
		r := requests[i]
		if r.ID <= 0 || r.From == r.To || !b.userExists(r.From) || !b.userExists(r.To) {
			b.note(requestTable, "invalid_friend_request")
			continue
		}
		status := resolution(r.Status)
		if status == "" || (r.Type != 0 && r.Type != 1 && r.Type != 2) {
			b.note(requestTable, "unknown_friend_request_state")
			continue
		}
		kind := "add"
		if r.Type == 0 || r.Type == 2 {
			kind = "recovery"
		}
		key := orderedPair(r.From, r.To)
		if status == "pending" {
			if pending[key] || active[key] || blocked[key] {
				status = "cancelled"
				b.note(requestTable, "ineligible_pending_request_cancelled")
			} else {
				pending[key] = true
			}
		}
		created := b.wallTime(requestTable, r.Created, b.wall)
		b.friendRequests = append(b.friendRequests, requestRow{ID: r.ID, From: r.From, To: r.To, Kind: kind, Status: status,
			Text: b.keepText(requestTable, r.Message), Created: created, Resolved: b.resolveTime(requestTable, status, r.Handled, created)})
	}
}

func (b *builder) mapFriendDeletion(r sourceFriend, users pair) {
	const table = "friend_relation"
	if r.Actor != users.Low && r.Actor != users.High {
		b.note(table, "invalid_friend_deletion_actor")
		return
	}
	deleted, err := time.ParseInLocation("2006-01-02 15:04:05", r.Deleted, b.wall)
	if err != nil || deleted.IsZero() {
		b.note(table, "invalid_friend_deletion_time")
		return
	}
	deleted = deleted.UTC()
	created := earlier(b.wallTime(table, r.Created, b.wall), deleted)
	if b.maxNotice > math.MaxInt64-2 {
		b.block(table, "notification_id_space_exhausted")
		return
	}
	b.maxNotice++
	b.deletions = append(b.deletions, friendDeletionRow{Pair: users, Notice: b.maxNotice, Actor: r.Actor, FriendshipCreated: created})
	peer := users.Low
	if peer == r.Actor {
		peer = users.High
	}
	for _, party := range [][2]int64{{r.Actor, peer}, {peer, r.Actor}} {
		participant, other := party[0], party[1]
		n := noticeRow{ID: b.maxNotice, User: participant, Kind: "friend.deleted", Created: deleted,
			Data: map[string]any{"user_id": other, "deleted_by": r.Actor}}
		if r.Read != 0 {
			n.Read = &deleted
		}
		b.notices = append(b.notices, n)
		if participant == r.Actor {
			b.maxNotice++
		}
	}
}

func resolution(value int64) string {
	switch value {
	case 0:
		return "pending"
	case 1:
		return "accepted"
	case 2:
		return "rejected"
	case 3:
		return "cancelled"
	default:
		return ""
	}
}

func (b *builder) resolveTime(table, status, handled string, created time.Time) *time.Time {
	if status == "pending" {
		return nil
	}
	if handled != "" {
		t := b.wallTime(table, handled, b.wall)
		return &t
	}
	b.note(table, "resolution_time_uses_creation")
	return &created
}

func (b *builder) mapGroupRequests(requests []sourceGroupRequest, transfers []sourceTransfer) {
	const requestTable = "chat_room_apply"
	for _, r := range requests {
		group := b.group(r.Room)
		if r.ID <= 0 || group == nil || !b.userExists(r.User) {
			b.note(requestTable, "invalid_group_request")
			continue
		}
		if r.Type != 1 {
			if r.Type == 2 {
				// v1 stores the invitee and free-text display name, never an
				// inviter UID. Guessing from the room owner would grant authority.
				b.note(requestTable, "group_invitation_inviter_unavailable")
			} else {
				b.note(requestTable, "unsupported_group_request_type")
			}
			continue
		}
		status := resolution(r.Status)
		if status == "" {
			b.note(requestTable, "unknown_group_request_state")
			continue
		}
		_, alreadyMember := b.memberKey[memberKey(r.Room, r.User)]
		if status == "pending" && (alreadyMember || group.Archived != nil) {
			status = "cancelled"
			b.note(requestTable, "ineligible_pending_request_cancelled")
		}
		created := b.wallTime(requestTable, r.Created, b.wall)
		b.groupRequests = append(b.groupRequests, requestRow{ID: r.ID, Room: r.Room, To: r.User, Kind: "application", Text: b.keepText(requestTable, r.Answer),
			Status: status, Created: created, Resolved: b.resolveTime(requestTable, status, "", created)})
	}
	const transferTable = "chat_room_transfer"
	pending := make(map[int64]bool)
	for i := len(transfers) - 1; i >= 0; i-- {
		r := transfers[i]
		group := b.group(r.Room)
		if r.ID <= 0 || group == nil || r.From == r.To || !b.userExists(r.From) || !b.userExists(r.To) {
			b.note(transferTable, "invalid_group_transfer")
			continue
		}
		status := resolution(r.Status)
		if status == "" {
			b.note(transferTable, "unknown_group_transfer_state")
			continue
		}
		created := b.wallTime(transferTable, r.Created, b.wall)
		_, member := b.memberKey[memberKey(r.Room, r.To)]
		if status == "pending" {
			if pending[r.Room] || !member || group.Archived != nil || group.Owner != r.From {
				status = "cancelled"
				b.note(transferTable, "ineligible_pending_transfer_cancelled")
			} else {
				pending[r.Room] = true
			}
		}
		b.transfers = append(b.transfers, requestRow{ID: r.ID, Room: r.Room, From: r.From, To: r.To, Status: status,
			Created: created, Resolved: b.resolveTime(transferTable, status, "", created)})
	}
}

func (b *builder) mapNotices(notices []sourceNotice) {
	const table = "chat_user_notice"
	for _, n := range notices {
		b.maxNotice = max(b.maxNotice, n.ID)
		if n.ID <= 0 || !b.userExists(n.User) {
			b.note(table, "invalid_notice_recipient")
			continue
		}
		created := b.wallTime(table, n.Created, b.wall)
		row := noticeRow{ID: n.ID, User: n.User, Kind: "legacy.notice", Created: created,
			Data: map[string]any{"title": b.keepText(table, n.Title), "content": b.keepText(table, n.Body), "link": b.keepText(table, n.Link)}}
		if n.Read != 0 {
			row.Read = &created
			b.note(table, "notice_read_time_uses_creation")
		}
		b.notices = append(b.notices, row)
	}
}

func sortedPairs(directs map[pair]time.Time) []pair {
	keys := make([]pair, 0, len(directs))
	for k := range directs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Low == keys[j].Low {
			return keys[i].High < keys[j].High
		}
		return keys[i].Low < keys[j].Low
	})
	return keys
}
