package legacy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"golang.org/x/crypto/bcrypt"
)

const reportVersion = 1
const DefaultWallTimeZone = "Asia/Shanghai"
const defaultMediaLimit = 256 << 20

// Options captured in the review determine all time and authority decisions.
type Options struct {
	AsOf          time.Time `json:"as_of"`
	WallTimeZone  string    `json:"wall_time_zone"`
	MediaOrigin   string    `json:"media_origin"`
	MaxMediaBytes int64     `json:"max_media_bytes"`
}

type TableReview struct {
	Name        string `json:"table"`
	Engine      string `json:"engine"`
	Disposition string `json:"disposition"`
	Rows        int64  `json:"rows"`
}

type Finding struct {
	Table    string `json:"table"`
	Code     string `json:"code"`
	Count    int64  `json:"count"`
	Blocking bool   `json:"blocking,omitempty"`
}

// Report carries aggregate counts only. The fingerprint binds values in the
// mapped tables and verified local media. Excluded tables contribute counts.
type Report struct {
	Version      int              `json:"version"`
	Options      Options          `json:"options"`
	Fingerprint  string           `json:"fingerprint"`
	SourceTables []TableReview    `json:"source_tables"`
	Planned      map[string]int64 `json:"planned"`
	Findings     []Finding        `json:"findings"`
}

func (r Report) json() ([]byte, error) { return json.Marshal(r) }

func (r Report) Blocked() bool {
	return slices.ContainsFunc(r.Findings, func(f Finding) bool { return f.Blocking })
}

func ReadReport(reader io.Reader) (Report, error) {
	var r Report
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return r, errors.New("invalid review JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return r, errors.New("unexpected data after review JSON")
	}
	if _, err := hex.DecodeString(r.Fingerprint); err != nil || r.Version != reportVersion || r.Options.AsOf.IsZero() || len(r.Fingerprint) != 64 {
		return r, errors.New("unsupported or incomplete review file")
	}
	return r, nil
}

type accountRow struct {
	ID                                                     int64
	Username, Nickname, Password, Email, Status, BanReason string
	Pat                                                    string
	AutoJoin, FirstLogin, Grant                            bool
	Created                                                time.Time
	BannedUntil, DeletedAt, PurgedAt, LastActive           *time.Time
	Avatar                                                 pgtype.UUID
}

type conversationRow struct {
	ID, Owner, Low, High                                 int64
	Kind, Title, Intro, Notice, Visibility, Policy, Code string
	Question, FixedCode, SiteBanReason                   string
	AllowInvite, SingleUse, Discoverable                 bool
	Archived, SiteBanUntil, TransferCooldownUntil        *time.Time
	Created, LastSent                                    time.Time
	Head                                                 int64
	Avatar                                               pgtype.UUID
}

type memberRow struct {
	Conversation, User, Read int64
	Role, Title              string
	Level                    int16
	Joined                   time.Time
	Mute                     *time.Time
	Hidden, LevelCustom      bool
}

type pair struct{ Low, High int64 }

func orderedPair(a, b int64) pair { return pair{min(a, b), max(a, b)} }

type friendshipRow struct {
	Pair    pair
	Created time.Time
}

type friendDeletionRow struct {
	Pair              pair
	Notice, Actor     int64
	FriendshipCreated time.Time
}

type preferenceRow struct {
	Owner, Other int64
	Blocked      bool
	Remark       string
}

type requestRow struct {
	ID, From, To, Room int64
	Kind, Text, Status string
	Created            time.Time
	Resolved           *time.Time
}

type messageRow struct {
	Table                               string
	SourceID, Conversation, Seq, Sender int64
	Kind, Body, RecallKind              string
	Attachment                          pgtype.UUID
	Duration                            *int32
	Reply, RecalledBy                   int64
	Mentions                            []int64
	Created                             time.Time
	Recalled                            *time.Time
}

type featuredRow struct {
	Conversation, Seq, Actor int64
	Created                  time.Time
}

type noticeRow struct {
	ID, User int64
	Kind     string
	Data     map[string]any
	Created  time.Time
	Read     *time.Time
}

type postRow struct {
	ID, Author, Parent, ReplyTo int64
	Body                        string
	Created                     time.Time
}

type postMediaRow struct {
	Post     int64
	Position int16
	Media    pgtype.UUID
}

type likeRow struct {
	Post, User int64
	Created    time.Time
}

type folderRow struct {
	ID, Room int64
	Name     string
	Created  time.Time
}

type groupFileRow struct {
	ID, Room, Owner, Folder int64
	Media                   pgtype.UUID
	Created                 time.Time
}

type moderationRow struct {
	ID, Reporter, Target             int64
	Kind, Title, Reason, State, Note string
	Anonymous                        bool
	Created, Updated                 time.Time
}
type stickerRow struct {
	Code, Label string
	Media       pgtype.UUID
}

// Plan owns a private in-memory snapshot of mapped values. Only Report should
// be written to a review file.
type Plan struct {
	report             Report
	sourceMedia        string
	accounts           []accountRow
	conversations      []conversationRow
	members            []memberRow
	friends            []friendshipRow
	deletions          []friendDeletionRow
	preferences        []preferenceRow
	friendRequests     []requestRow
	groupRequests      []requestRow
	transfers          []requestRow
	messages           []messageRow
	featured           []featuredRow
	notices            []noticeRow
	posts              []postRow
	postMedia          []postMediaRow
	likes              []likeRow
	folders            []folderRow
	groupFiles         []groupFileRow
	stickers           []stickerRow
	reports            []moderationRow
	media              []mediaItem
	maxRoom, maxNotice int64
	legacyMaxID        map[string]int64
}

func (p *Plan) Report() Report {
	r := p.report
	r.SourceTables = slices.Clone(r.SourceTables)
	r.Findings = slices.Clone(r.Findings)
	r.Planned = maps.Clone(r.Planned)
	return r
}

func (p *Plan) matches(review Report) bool {
	a, errA := p.report.json()
	b, errB := review.json()
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func Prepare(ctx context.Context, dsn, sourceMedia string, opts Options) (*Plan, error) {
	if _, _, err := normalizeOptions(opts); err != nil {
		return nil, err
	}
	s, err := capture(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return buildPlan(ctx, s, opts, sourceMedia)
}

func normalizeOptions(opts Options) (Options, *time.Location, error) {
	if opts.AsOf.IsZero() {
		opts.AsOf = time.Now()
	}
	opts.AsOf = opts.AsOf.UTC().Truncate(time.Second)
	if opts.WallTimeZone == "" {
		opts.WallTimeZone = DefaultWallTimeZone
	}
	if opts.WallTimeZone == "Local" {
		return opts, nil, errors.New("use an explicit legacy wall-clock time zone instead of Local")
	}
	loc, err := time.LoadLocation(opts.WallTimeZone)
	if err != nil {
		return opts, nil, errors.New("invalid legacy wall-clock time zone")
	}
	if opts.AsOf.Year() < 1970 || opts.AsOf.Year() > 9999 {
		return opts, nil, errors.New("invalid snapshot time")
	}
	if opts.MaxMediaBytes == 0 {
		opts.MaxMediaBytes = defaultMediaLimit
	}
	if opts.MaxMediaBytes < 1 || opts.MaxMediaBytes > 1<<40 {
		return opts, nil, errors.New("max media size must be between 1 byte and 1 TiB")
	}
	if opts.MediaOrigin != "" {
		u, err := url.Parse(opts.MediaOrigin)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return opts, nil, errors.New("media origin must contain only the legacy HTTP(S) scheme, host and optional port")
		}
		opts.MediaOrigin = u.Scheme + "://" + strings.ToLower(u.Host)
	}
	return opts, loc, nil
}

type builder struct {
	*Plan
	ctx       context.Context
	wall      *time.Location
	source    *mediaSource
	mediaErr  error
	findings  map[Finding]int64
	users     map[int64]int
	rooms     map[int64]int
	memberKey map[[2]int64]int
}

func buildPlan(ctx context.Context, s *snapshot, opts Options, src string) (*Plan, error) {
	opts, wall, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	source, err := openMediaSource(src, opts)
	if err != nil {
		return nil, err
	}
	defer source.close()
	b := &builder{
		Plan: &Plan{report: Report{Version: reportVersion, Options: opts, SourceTables: slices.Clone(s.tables)}, sourceMedia: src},
		ctx:  ctx, wall: wall, source: source, findings: make(map[Finding]int64),
		users: make(map[int64]int), rooms: make(map[int64]int), memberKey: make(map[[2]int64]int),
	}
	b.legacyMaxID = legacyIdentityCeilings(s)
	if s.tables != nil {
		b.reviewMissingTables(s.tables)
	}
	b.mapAccounts(s.users)
	b.mapGrants(s.grants)
	b.mapGroups(s.rooms, s.members, s.admins)
	b.mapNotices(s.notices)
	b.mapFriends(s.friends, s.friendRequests)
	b.mapGroupRequests(s.groupRequests, s.transfers)
	b.mapStickers(s.stickers)
	b.mapHistory(s.groupMessages, s.privateMessages, s.features, s.users)
	b.mapSpace(s.posts)
	b.mapGroupFiles(s.rooms, s.files)
	b.mapReports(s.reports)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.mediaErr != nil {
		return nil, b.mediaErr
	}
	return b.finish(s.digest)
}

func legacyIdentityCeilings(s *snapshot) map[string]int64 {
	ceilings := make(map[string]int64)
	for _, r := range s.friendRequests {
		ceilings["friend_requests"] = max(ceilings["friend_requests"], r.ID)
	}
	for _, r := range s.groupRequests {
		ceilings["group_join_requests"] = max(ceilings["group_join_requests"], r.ID)
	}
	for _, r := range s.transfers {
		ceilings["group_transfers"] = max(ceilings["group_transfers"], r.ID)
	}
	for _, r := range s.posts {
		ceilings["space_posts"] = max(ceilings["space_posts"], r.ID)
	}
	for _, r := range s.files {
		ceilings["group_files"] = max(ceilings["group_files"], r.ID)
	}
	for _, r := range s.reports {
		ceilings["reports"] = max(ceilings["reports"], r.ID)
	}
	return ceilings
}

func (b *builder) reviewMissingTables(tables []TableReview) {
	for _, name := range []string{"chat_essence", "chat_report", "chat_room_apply", "chat_room_transfer", "csac_registration_code_redemptions", "emoji_list", "friend_request", "group_files", "space_shc"} {
		if !slices.ContainsFunc(tables, func(t TableReview) bool { return t.Name == name }) {
			b.note(name, "optional_table_absent")
		}
	}
	for _, t := range tables {
		if t.Disposition == "mapped" && t.Engine != "InnoDB" {
			b.note(t.Name, "requires_quiesced_source")
		}
	}
}

func (b *builder) finish(sourceDigest string) (*Plan, error) {
	b.report.Findings = make([]Finding, 0, len(b.findings))
	for finding, n := range b.findings {
		finding.Count = n
		b.report.Findings = append(b.report.Findings, finding)
	}
	sort.Slice(b.report.Findings, func(i, j int) bool {
		a, z := b.report.Findings[i], b.report.Findings[j]
		if a.Table != z.Table {
			return a.Table < z.Table
		}
		return a.Code < z.Code
	})
	b.report.Planned = map[string]int64{}
	for name, count := range map[string]int{
		"accounts": len(b.accounts), "conversations": len(b.conversations), "conversation_members": len(b.members),
		"conversation_heads": len(b.conversations), "friendships": len(b.friends), "friend_preferences": len(b.preferences),
		"friend_deletions": len(b.deletions), "friend_requests": len(b.friendRequests), "group_join_requests": len(b.groupRequests), "group_transfers": len(b.transfers),
		"messages": len(b.messages), "featured_messages": len(b.featured), "notifications": len(b.notices),
		"space_posts": len(b.posts), "space_post_media": len(b.postMedia), "space_likes": len(b.likes),
		"group_file_folders": len(b.folders), "group_files": len(b.groupFiles), "stickers": len(b.stickers),
		"media_objects": len(b.media), "legacy_message_map": len(b.messages), "reports": len(b.reports),
	} {
		b.report.Planned[name] = int64(count)
	}
	for _, name := range []string{"chat_msg_messages", "private_msg_messages", "direct_conversations", "group_conversations",
		"recalled_messages", "message_replies", "media_bytes", "accounts_with_legacy_credentials", "registration_grants", "user_activity"} {
		b.report.Planned[name] = 0
	}
	for _, a := range b.accounts {
		if a.LastActive != nil {
			b.report.Planned["user_activity"]++
		}
		if a.Password != "" {
			b.report.Planned["accounts_with_legacy_credentials"]++
		}
		if a.Grant {
			b.report.Planned["registration_grants"]++
		}
	}
	for _, c := range b.conversations {
		b.report.Planned[c.Kind+"_conversations"]++
	}
	groupParticipants := make(map[[2]int64]struct{})
	for _, m := range b.messages {
		b.report.Planned[m.Table+"_messages"]++
		if m.Table == "chat_msg" && m.Kind != "nudge" {
			groupParticipants[memberKey(m.Conversation, m.Sender)] = struct{}{}
		}
		if m.Recalled != nil {
			b.report.Planned["recalled_messages"]++
		}
		if m.Reply != 0 {
			b.report.Planned["message_replies"]++
		}
	}
	b.report.Planned["group_member_experience"] = int64(len(groupParticipants))
	for _, media := range b.media {
		b.report.Planned["media_bytes"] += media.Size
		b.report.Planned["media_"+media.Purpose]++
	}
	digest := sha256.New()
	enc := json.NewEncoder(digest)
	if err := enc.Encode(sourceDigest); err != nil {
		return nil, err
	}
	if err := enc.Encode(b.report); err != nil {
		return nil, err
	}
	for _, m := range b.media {
		if err := enc.Encode(m); err != nil {
			return nil, err
		}
	}
	b.report.Fingerprint = hex.EncodeToString(digest.Sum(nil))
	return b.Plan, nil
}

func (b *builder) note(table, code string) { b.findings[Finding{Table: table, Code: code}]++ }
func (b *builder) block(table, code string) {
	b.findings[Finding{Table: table, Code: code, Blocking: true}]++
}

func (b *builder) userExists(id int64) bool { _, ok := b.users[id]; return ok }

func (b *builder) wallTime(table, value string, zone *time.Location) time.Time {
	parsed, err := time.ParseInLocation("2006-01-02 15:04:05", value, zone)
	if err != nil || parsed.IsZero() {
		b.note(table, "invalid_time_uses_cutover")
		return b.report.Options.AsOf
	}
	return parsed.UTC()
}

func (b *builder) epochTime(table string, value int64) time.Time {
	if value <= 0 {
		b.note(table, "invalid_time_uses_cutover")
		return b.report.Options.AsOf
	}
	return time.Unix(value, 0).UTC()
}

func optionalEpoch(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	t := time.Unix(value, 0).UTC()
	return &t
}

func sqlText(s string) string { return strings.ReplaceAll(s, "\x00", "\uFFFD") }

func boundedUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

func asciiFold(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

func supportedCredential(hash string) bool {
	if len(hash) == 32 || len(hash) == 64 {
		_, err := hex.DecodeString(hash)
		return err == nil
	}
	hash = strings.TrimPrefix(strings.TrimPrefix(hash, "$csac$md5$"), "$csac$sha256$")
	if len(hash) != 60 || !(strings.HasPrefix(hash, "$2a$") || strings.HasPrefix(hash, "$2b$") || strings.HasPrefix(hash, "$2y$")) {
		return false
	}
	for _, c := range hash[7:] {
		if !strings.ContainsRune("./ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789", c) {
			return false
		}
	}
	cost, err := bcrypt.Cost([]byte(hash))
	return err == nil && cost <= 14
}

func (b *builder) mapAccounts(users []sourceUser) {
	const table = "chat_user"
	usernameKeys := make(map[string]struct{})
	emailKeys := make(map[string]struct{})
	for _, u := range users {
		if u.ID <= 0 || b.userExists(u.ID) || strings.TrimSpace(u.Username) == "" || strings.ContainsRune(u.Username, 0) {
			b.block(table, "invalid_account_identity")
			continue
		}
		key := asciiFold(u.Username)
		if _, exists := usernameKeys[key]; exists {
			b.block(table, "duplicate_username")
			continue
		}
		usernameKeys[key] = struct{}{}
		row := accountRow{ID: u.ID, Username: u.Username, Nickname: b.keepText(table, u.Nickname), FirstLogin: u.FirstLogin != 0,
			AutoJoin: u.AutoJoin != 0, Pat: b.keepText(table, u.Pat), Created: b.epochTime(table, u.Created),
			LastActive: optionalEpoch(u.LastActive), Status: "active"}
		if strings.TrimSpace(u.Username) != u.Username {
			b.note(table, "username_has_surrounding_space")
		}
		row.Email = strings.TrimSpace(u.Email)
		if strings.ContainsRune(row.Email, 0) {
			b.note(table, "invalid_email_dropped")
			row.Email = ""
		}
		if u.BanUntil > b.report.Options.AsOf.Unix() {
			row.Status, row.BannedUntil, row.BanReason = "banned", optionalEpoch(u.BanUntil), b.keepText(table, u.BanReason)
		}
		switch u.Status {
		case 1:
		case 2, 3:
			row.Status, row.BannedUntil, row.BanReason = "deleted", nil, ""
			row.DeletedAt = optionalEpoch(u.Deleted)
			if u.Status == 2 && row.DeletedAt == nil {
				b.note(table, "deletion_time_unavailable")
			}
		default:
			b.block(table, "unknown_account_status")
			continue
		}
		if u.Status == 3 {
			row.PurgedAt = row.DeletedAt
			if row.PurgedAt == nil {
				row.PurgedAt = &b.report.Options.AsOf
			}
			row.Email = ""
		} else {
			if supportedCredential(u.Password) {
				row.Password = u.Password
			} else {
				b.note(table, "credential_reset_required")
			}
			if u.Avatar != "" {
				row.Avatar = b.attach(mediaRef{Table: table, ID: u.ID, Owner: u.ID, Path: u.Avatar, Scope: "user-avatar", Created: row.Created})
			}
		}
		if u.Bot != 0 {
			b.note(table, "bot_runtime_not_mapped")
		}
		if row.Email != "" {
			key := asciiFold(row.Email)
			if _, exists := emailKeys[key]; exists {
				b.block(table, "duplicate_email")
			}
			emailKeys[key] = struct{}{}
		}
		b.users[u.ID] = len(b.accounts)
		b.accounts = append(b.accounts, row)
	}
}

func (b *builder) mapGrants(grants []sourceGrant) {
	const table = "csac_registration_code_redemptions"
	for _, grant := range grants {
		i, exists := b.users[grant.User]
		if !exists || b.accounts[i].PurgedAt != nil {
			b.note(table, "invalid_registration_grant_recipient")
			continue
		}
		b.accounts[i].Grant = true
	}
}

func csvIDs(s string) ([]int64, int) {
	var ids []int64
	bad := 0
	seen := make(map[int64]bool)
	for token := range strings.SplitSeq(s, ",") {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}
		id, err := strconv.ParseInt(token, 10, 64)
		if err != nil || id <= 0 || seen[id] {
			bad++
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, bad
}

func memberKey(room, user int64) [2]int64 { return [2]int64{room, user} }

func (b *builder) addMember(m memberRow) {
	b.memberKey[memberKey(m.Conversation, m.User)] = len(b.members)
	b.members = append(b.members, m)
}

func (b *builder) group(id int64) *conversationRow {
	if i, ok := b.rooms[id]; ok {
		return &b.conversations[i]
	}
	return nil
}

func (b *builder) keepText(table, value string) string {
	if strings.ContainsRune(value, 0) {
		b.note(table, "nul_text_replaced")
	}
	return sqlText(value)
}

func (b *builder) reportN(table, code string, n int) {
	if n > 0 {
		b.findings[Finding{Table: table, Code: code}] += int64(n)
	}
}

func earlier(a, z time.Time) time.Time {
	if a.IsZero() || z.Before(a) {
		return z
	}
	return a
}

func (b *builder) finishTimes() {
	for i := range b.conversations {
		c := &b.conversations[i]
		if c.Created.IsZero() {
			if c.Kind == "group" {
				b.note("chat_room", "group_creation_time_unavailable")
			}
			c.Created = b.report.Options.AsOf
		}
		if c.LastSent.IsZero() {
			c.LastSent = c.Created
		}
	}
}

func ptrIfPositive(v int64) *int64 {
	if v <= 0 {
		return nil
	}
	return &v
}

func nonempty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func formatRowsMismatch(table string, count, expected int64) error {
	return fmt.Errorf("destination %s copied %d rows, expected %d", table, count, expected)
}
