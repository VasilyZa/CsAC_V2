package legacy

import (
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

func (b *builder) mapSpace(posts []sourcePost) {
	const table = "space_shc"
	byID := make(map[int64]int, len(posts))
	roots := make(map[int64]int64, len(posts))
	for i, p := range posts {
		if p.ID <= 0 || !b.userExists(p.Author) {
			continue
		}
		if _, exists := byID[p.ID]; exists {
			b.block(table, "duplicate_space_identity")
		}
		byID[p.ID] = i
		if p.Reply == 0 {
			roots[p.ID] = p.ID
		}
	}
	for _, p := range posts {
		if _, exists := byID[p.ID]; !exists || !b.userExists(p.Author) {
			b.note(table, "invalid_space_post")
			continue
		}
		var parent, replyTo int64
		switch p.Reply {
		case 0:
			if p.Parent != 0 {
				b.note(table, "parent_on_root_ignored")
			}
		case 1:
			parent = legacySpaceRoot(p.ID, posts, byID, roots)
			switch parent {
			case spaceCyclic:
				b.note(table, "cyclic_space_reply")
				continue
			case spaceOrphan:
				b.note(table, "orphan_space_parent")
				continue
			}
			if p.Parent != parent {
				replyTo = p.Parent
			}
		default:
			b.note(table, "invalid_space_reply_flag")
			continue
		}
		created := b.wallTime(table, p.Created, b.wall)
		body := b.keepText(table, p.Body)
		if len(body) > 4000 {
			body = boundedUTF8(body, 4000)
			b.note(table, "space_content_truncated")
		}
		b.posts = append(b.posts, postRow{ID: p.ID, Author: p.Author, Parent: parent, ReplyTo: replyTo, Body: body, Created: created})
		users, invalid := csvIDs(p.Likes)
		b.reportN(table, "invalid_or_duplicate_space_like", invalid)
		if int64(len(users)) != p.LikeCount {
			b.note(table, "space_like_count_disagrees_with_ids")
		}
		for _, user := range users {
			if !b.userExists(user) {
				b.note(table, "orphan_space_like")
				continue
			}
			b.likes = append(b.likes, likeRow{Post: p.ID, User: user, Created: created})
		}
		if p.Images == "" {
			continue
		}
		var paths []string
		if err := json.Unmarshal([]byte(p.Images), &paths); err != nil {
			b.note(table, "invalid_space_media_list")
			continue
		}
		seen := make(map[string]bool)
		var position int16
		for i, path := range paths {
			if seen[path] {
				b.note(table, "duplicate_space_media")
				continue
			}
			seen[path] = true
			if position >= 9 {
				b.note(table, "space_media_limit")
				continue
			}
			id := b.attach(mediaRef{Table: table, ID: p.ID, Owner: p.Author, Position: i, Scope: "space-image", Path: path, Created: created})
			if id.Valid {
				b.postMedia = append(b.postMedia, postMediaRow{Post: p.ID, Position: position, Media: id})
				position++
			}
		}
	}
}

const (
	spaceVisiting int64 = -1
	spaceCyclic   int64 = -2
	spaceOrphan   int64 = -3
)

// Memoize the root of each walked chain. Use an iterative walk to handle long
// imported threads and cycles without recursion or repeated ancestor scans.
func legacySpaceRoot(start int64, posts []sourcePost, byID map[int64]int, roots map[int64]int64) int64 {
	current := start
	var root int64
	for {
		if known, ok := roots[current]; ok {
			root = known
			if known == spaceVisiting {
				root = spaceCyclic
			}
			break
		}
		i, present := byID[current]
		if !present || posts[i].Reply != 1 {
			root = spaceOrphan
			break
		}
		roots[current] = spaceVisiting
		current = posts[i].Parent
	}
	for current := start; roots[current] == spaceVisiting; current = posts[byID[current]].Parent {
		roots[current] = root
	}
	return root
}

type folderKey struct {
	room int64
	name string
}

func (b *builder) mapGroupFiles(rooms []sourceRoom, files []sourceFile) {
	const table = "group_files"
	folders := make(map[folderKey]int64)
	addFolder := func(room int64, name, source string, created time.Time) int64 {
		name = strings.TrimSpace(name)
		if name == "" {
			return 0
		}
		if utf8.RuneCountInString(name) > 50 || strings.ContainsRune(name, 0) {
			b.note(source, "unsupported_folder_name")
			return 0
		}
		key := folderKey{room: room, name: name}
		if id := folders[key]; id != 0 {
			return id
		}
		id := int64(len(b.folders)) + 1
		folders[key] = id
		b.folders = append(b.folders, folderRow{ID: id, Room: room, Name: name, Created: created})
		return id
	}
	for _, room := range rooms {
		group := b.group(room.ID)
		if group == nil || room.Folders == "" {
			continue
		}
		var names []string
		if err := json.Unmarshal([]byte(room.Folders), &names); err != nil {
			b.note("chat_room", "invalid_group_folder_list")
			continue
		}
		for _, name := range names {
			addFolder(room.ID, name, "chat_room", group.Created)
		}
	}
	for _, f := range files {
		if b.group(f.Room) == nil || !b.userExists(f.Owner) || f.ID <= 0 {
			b.note(table, "invalid_group_file_reference")
			continue
		}
		created := b.epochTime(table, f.Created)
		folder := addFolder(f.Room, f.Folder, table, created)
		id := b.attach(mediaRef{Table: table, ID: f.ID, Owner: f.Owner, Room: f.Room, Scope: "group-file", Created: created,
			Path: f.Path, Name: f.Name, ExpectedSize: f.Size, Expiry: optionalEpoch(f.Expiry)})
		if id.Valid {
			b.groupFiles = append(b.groupFiles, groupFileRow{ID: f.ID, Room: f.Room, Owner: f.Owner, Folder: folder,
				Media: id, Created: created})
		}
	}
}

func (b *builder) mapReports(reports []sourceReport) {
	const table = "chat_report"
	for _, report := range reports {
		if report.ID <= 0 || report.Target <= 0 || (report.Kind != "user" && report.Kind != "group") {
			b.note(table, "invalid_report_identity_or_target")
			continue
		}
		reason := b.keepText(table, report.Reason)
		if reason == "" {
			b.note(table, "empty_report_reason")
			continue
		}
		if len(reason) > 12000 {
			reason = boundedUTF8(reason, 12000)
			b.note(table, "report_reason_truncated")
		}
		if report.Kind == "user" && !b.userExists(report.Target) || report.Kind == "group" && b.group(report.Target) == nil {
			b.note(table, "report_target_no_longer_exists")
		}
		state := report.Status
		switch state {
		case "pending", "processing", "resolved":
		default:
			state = "pending"
			b.note(table, "unknown_report_state_uses_pending")
		}
		created := b.epochTime(table, report.Created)
		row := moderationRow{ID: report.ID, Target: report.Target, Kind: report.Kind, Title: b.keepText(table, report.Name),
			Reason: reason, State: state, Note: b.keepText(table, report.Reply), Anonymous: report.Anonymous != 0,
			Created: created, Updated: created}
		if !row.Anonymous {
			if b.userExists(report.Reporter) {
				row.Reporter = report.Reporter
			} else {
				b.note(table, "missing_reporter_left_unlinked")
			}
		}
		if updated := optionalEpoch(report.Processed); updated != nil && !updated.Before(created) {
			row.Updated = *updated
		} else if state != "pending" || report.Processed != 0 {
			b.note(table, "report_update_time_uses_creation")
		}
		b.reports = append(b.reports, row)
	}
}
