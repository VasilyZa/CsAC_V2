package legacy

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	groupMessageTable   = "chat_msg"
	privateMessageTable = "private_msg"
)

type messageKey struct {
	Table string
	ID    int64
}

type pendingMessage struct {
	Row                 messageRow
	LegacyReply, Recall int64
	Duration            int64
	Image, Voice, Meta  string
}

func (b *builder) mapHistory(groups []sourceGroupMessage, private []sourcePrivateMessage, features []sourceFeature, users []sourceUser) {
	directs := make(map[pair]time.Time)
	for _, f := range b.friends {
		directs[f.Pair] = f.Created
	}
	for _, m := range private {
		if m.Type == "private" && b.validDirect(m) {
			key := orderedPair(m.From, m.To)
			created := b.report.Options.AsOf
			if m.Created > 0 {
				created = time.Unix(m.Created, 0).UTC()
			}
			directs[key] = earlier(directs[key], created)
		}
	}
	if int64(len(directs)) > math.MaxInt64-b.maxRoom {
		b.block("chat_room", "no_available_direct_conversation_ids")
		return
	}
	directIndex := make(map[pair]int)
	for i, key := range sortedPairs(directs) {
		id := b.maxRoom + int64(i) + 1
		directIndex[key] = len(b.conversations)
		b.conversations = append(b.conversations, conversationRow{ID: id, Kind: "direct", Low: key.Low, High: key.High,
			Visibility: "private", Policy: "invite", Created: directs[key]})
		b.addMember(memberRow{Conversation: id, User: key.Low, Role: "member", Joined: directs[key], Level: 1})
		b.addMember(memberRow{Conversation: id, User: key.High, Role: "member", Joined: directs[key], Level: 1})
	}
	// Each SELECT is ordered by its table's primary key. Different rooms can
	// be interleaved, so each conversation has its own dense head and ID list.
	groupIDs := make(map[int64][]int64)
	var pending []pendingMessage
	for _, m := range groups {
		c := b.group(m.Room)
		if c == nil || m.ID <= 0 {
			b.note(groupMessageTable, "invalid_message_room_or_id")
			continue
		}
		if !b.userExists(m.Sender) {
			b.note(groupMessageTable, "orphan_message_sender")
			continue
		}
		created := b.wallTime(groupMessageTable, m.Created, time.UTC)
		row := b.nextMessage(c, groupMessageTable, m.ID, m.Sender, m.Kind, m.Body, created)
		mentionIDs, invalid := csvIDs(m.Mentions)
		b.reportN(groupMessageTable, "invalid_or_duplicate_mention", invalid)
		for _, id := range mentionIDs {
			if b.userExists(id) {
				row.Mentions = append(row.Mentions, id)
			} else {
				b.note(groupMessageTable, "orphan_mention")
			}
		}
		pending = append(pending, pendingMessage{Row: row, LegacyReply: m.Reply, Recall: m.Recall,
			Duration: m.Duration, Image: m.Body, Voice: m.Body, Meta: m.FileMeta})
		groupIDs[c.ID] = append(groupIDs[c.ID], m.ID)
	}
	for i := range b.members {
		member := &b.members[i]
		if b.group(member.Conversation) == nil || member.Read == 0 {
			continue
		}
		ids := groupIDs[member.Conversation]
		read := sort.Search(len(ids), func(i int) bool { return ids[i] > member.Read })
		if read == 0 || ids[read-1] != member.Read {
			b.note("chat_group_user", "group_read_uses_preceding_message")
		}
		member.Read = int64(read)
	}
	firstUnread := make(map[[2]int64]int64)
	for _, m := range private {
		switch m.Type {
		case "system":
			b.mapPrivateNotice(m)
		case "private":
			if !b.validDirect(m) {
				b.note(privateMessageTable, "invalid_direct_endpoints_or_id")
				continue
			}
			if m.Room != 0 {
				b.note(privateMessageTable, "private_room_id_ignored")
			}
			c := &b.conversations[directIndex[orderedPair(m.From, m.To)]]
			created := b.epochTime(privateMessageTable, m.Created)
			row := b.nextMessage(c, privateMessageTable, m.ID, m.From, m.Kind, m.Body, created)
			pending = append(pending, pendingMessage{Row: row, LegacyReply: m.Reply, Recall: m.Recall, Duration: m.Duration,
				Image: m.Image, Voice: m.Voice, Meta: m.FileMeta})
			key := memberKey(c.ID, m.To)
			if m.Read == 0 && firstUnread[key] == 0 {
				firstUnread[key] = row.Seq
			} else if m.Read != 0 && firstUnread[key] != 0 {
				b.note(privateMessageTable, "non_prefix_direct_read")
			}
		default:
			b.note(privateMessageTable, "unknown_private_type")
		}
	}
	for _, i := range directIndex {
		c := b.conversations[i]
		for _, uid := range []int64{c.Low, c.High} {
			key := memberKey(c.ID, uid)
			read := c.Head
			if seq := firstUnread[key]; seq != 0 {
				read = seq - 1
			}
			b.members[b.memberKey[key]].Read = read
		}
	}
	b.finishTimes()
	b.mapHidden(users)
	mapped := make(map[messageKey]int, len(pending))
	for _, p := range pending {
		key := messageKey{Table: p.Row.Table, ID: p.Row.SourceID}
		if _, exists := mapped[key]; exists {
			b.block(p.Row.Table, "duplicate_message_identity")
		}
		mapped[key] = len(b.messages)
		b.messages = append(b.messages, p.Row)
	}
	for i, p := range pending {
		row := &b.messages[i]
		if p.LegacyReply != 0 {
			targetIndex, ok := mapped[messageKey{Table: row.Table, ID: p.LegacyReply}]
			if !ok || targetIndex == i || b.messages[targetIndex].Conversation != row.Conversation {
				b.note(row.Table, "invalid_reply")
			} else {
				row.Reply = b.messages[targetIndex].Seq
			}
		}
		b.mapMessagePayload(row, p)
	}
	b.mapFeatured(groups, features, mapped)
}

func (b *builder) validDirect(m sourcePrivateMessage) bool {
	return m.ID > 0 && m.From != m.To && b.userExists(m.From) && b.userExists(m.To)
}

func (b *builder) nextMessage(c *conversationRow, table string, id, sender, kind int64, body string, created time.Time) messageRow {
	c.Head++
	c.Created = earlier(c.Created, created)
	if c.LastSent.IsZero() || created.After(c.LastSent) {
		c.LastSent = created
	}
	var label string
	switch kind {
	case 1:
		label = "text"
	case 2:
		label = "image"
	case 3:
		label = "voice"
	case 4:
		label = "nudge"
	case 5:
		label = "sticker"
	case 6:
		label = "file"
	default:
		label = "text"
		b.note(table, "unknown_message_kind_uses_text")
	}
	return messageRow{Table: table, SourceID: id, Conversation: c.ID, Seq: c.Head, Sender: sender,
		Kind: label, Body: b.keepText(table, body), Created: created, Mentions: []int64{}}
}

func (b *builder) mapHidden(users []sourceUser) {
	for _, u := range users {
		if !b.userExists(u.ID) || u.Hide == "" {
			continue
		}
		// The v1 server receives group room IDs. Its clients keep directly
		// hidden chats separately in client-local state.
		ids, invalid := csvIDs(u.Hide)
		b.reportN("chat_user", "invalid_hidden_conversation", invalid)
		for _, gid := range ids {
			if i, ok := b.memberKey[memberKey(gid, u.ID)]; ok && b.group(gid) != nil {
				b.members[i].Hidden = true
			} else {
				b.note("chat_user", "unknown_hidden_conversation")
			}
		}
	}
}

func (b *builder) mapPrivateNotice(m sourcePrivateMessage) {
	if m.ID <= 0 || !b.userExists(m.To) {
		b.note(privateMessageTable, "invalid_system_notice_recipient")
		return
	}
	if b.maxNotice == math.MaxInt64 {
		b.block(privateMessageTable, "no_available_system_notice_ids")
		return
	}
	b.maxNotice++
	created := b.epochTime(privateMessageTable, m.Created)
	row := noticeRow{ID: b.maxNotice, User: m.To, Kind: "legacy.private_system", Created: created,
		Data: map[string]any{"content": b.keepText(privateMessageTable, m.Body), "legacy_id": strconv.FormatInt(m.ID, 10)}}
	if b.userExists(m.From) {
		row.Data["from_user_id"] = strconv.FormatInt(m.From, 10)
	}
	if m.Read != 0 {
		row.Read = &created
		b.note(privateMessageTable, "notice_read_time_uses_creation")
	}
	b.notices = append(b.notices, row)
	b.note(privateMessageTable, "system_message_mapped_to_notice")
}

type fileMetadata struct {
	Name      string `json:"file_name"`
	Size      int64  `json:"file_size"`
	Path      string `json:"file_path"`
	PublicURL string `json:"public_url"`
	Expiry    int64  `json:"expires_at"`
}

func (b *builder) mapMessagePayload(row *messageRow, input pendingMessage) {
	if input.Recall != 0 {
		row.Recalled = &row.Created
		b.note(row.Table, "recall_time_uses_creation")
		if input.Recall == 1 {
			row.RecalledBy = row.Sender
			row.RecallKind = "sender"
		} else {
			if row.Table == groupMessageTable {
				switch input.Recall {
				case 2:
					row.RecallKind = "admin"
				case 3:
					row.RecallKind = "owner"
				}
			}
			b.note(row.Table, "recall_actor_unavailable")
		}
		row.Body, row.Mentions = "", []int64{}
		return
	}
	ref := mediaRef{Table: row.Table, ID: row.SourceID, Owner: row.Sender, Room: row.Conversation,
		Created: row.Created, GroupMessage: row.Table == groupMessageTable}
	switch row.Kind {
	case "image":
		ref.Path, ref.Scope = input.Image, "message-image"
		row.Body = ""
	case "voice":
		ref.Path, ref.Scope = input.Voice, "message-voice"
		row.Body = ""
		if input.Duration >= 0 && input.Duration <= math.MaxInt32/1000 {
			ms := int32(input.Duration * 1000)
			row.Duration = &ms
		} else {
			b.note(row.Table, "invalid_voice_duration")
		}
	case "file":
		var meta fileMetadata
		if input.Meta == "" || json.Unmarshal([]byte(input.Meta), &meta) != nil {
			b.note(row.Table, "invalid_file_metadata")
			row.Body = ""
			return
		}
		ref.Scope, ref.Path, ref.Name, ref.ExpectedSize, ref.Expiry = "message-file", meta.Path, meta.Name, meta.Size, optionalEpoch(meta.Expiry)
		if ref.Path == "" {
			ref.Path = meta.PublicURL
		}
		row.Body = ""
	default:
		return
	}
	row.Attachment = b.attach(ref)
}

func (b *builder) mapFeatured(groups []sourceGroupMessage, features []sourceFeature, mapped map[messageKey]int) {
	const table = "chat_essence"
	seen := make(map[int64]bool)
	for _, f := range features {
		i, exists := mapped[messageKey{Table: groupMessageTable, ID: f.Message}]
		if !exists || b.messages[i].Conversation != f.Room || b.messages[i].Recalled != nil || !b.userExists(f.Actor) || seen[f.Message] {
			b.note(table, "invalid_featured_message")
			continue
		}
		seen[f.Message] = true
		b.featured = append(b.featured, featuredRow{Conversation: f.Room, Seq: b.messages[i].Seq, Actor: f.Actor,
			Created: b.wallTime(table, f.Created, b.wall)})
	}
	for _, group := range groups {
		if group.Featured != 0 && !seen[group.ID] {
			b.note(groupMessageTable, "feature_metadata_unavailable")
		}
	}
}

func (b *builder) mapStickers(stickers []sourceSticker) {
	const table = "emoji_list"
	seen := make(map[string]bool)
	for i, sticker := range stickers {
		if strings.TrimSpace(sticker.Code) == "" || strings.ContainsRune(sticker.Code, 0) || seen[sticker.Code] {
			b.note(table, "invalid_sticker_code")
			continue
		}
		seen[sticker.Code] = true
		row := stickerRow{Code: sticker.Code, Label: b.keepText(table, sticker.Label)}
		if sticker.Path != "" {
			if len(b.accounts) == 0 {
				b.note(table, "no_media_owner_for_sticker")
			} else {
				// Media owner is a required FK. Sticker reads are authorized by
				// the catalog, so ownership doesn't grant site-admin privileges.
				row.Media = b.attach(mediaRef{Table: table, ID: int64(i + 1), Owner: b.accounts[0].ID,
					Scope: "sticker", Path: sticker.Path, Created: b.report.Options.AsOf})
			}
		}
		b.stickers = append(b.stickers, row)
	}
}
