package account

// Keep the conversation IDs and sequence high-water marks. All content in a
// closed conversation is removed, including messages authored by other users.
const closedAccountConversations = `SELECT id FROM conversations
	WHERE (kind='group' AND owner_id=$1) OR (kind='direct' AND (direct_low=$1 OR direct_high=$1))`

const deletedAccountMessages = `SELECT conversation_id,seq FROM messages WHERE sender_id=$1
	UNION SELECT conversation_id,seq FROM messages WHERE nudge_target_id=$1
	UNION SELECT conversation_id,seq FROM messages WHERE system_subject_id=$1
	UNION SELECT m.conversation_id,m.seq FROM messages m JOIN (` + closedAccountConversations + `) c ON c.id=m.conversation_id`

const lockPurgeConversations = `WITH affected(id) AS (
	` + closedAccountConversations + `
	UNION SELECT conversation_id FROM conversation_members WHERE user_id=$1
	UNION SELECT conversation_id FROM group_join_requests WHERE user_id=$1 OR inviter_id=$1
	UNION SELECT conversation_id FROM group_transfers WHERE from_user_id=$1 OR to_user_id=$1
	UNION SELECT conversation_id FROM group_files WHERE uploader_id=$1
	UNION SELECT conversation_id FROM featured_messages WHERE added_by=$1
	UNION SELECT conversation_id FROM messages WHERE sender_id=$1 OR nudge_target_id=$1 OR recalled_by=$1
	UNION SELECT conversation_id FROM messages WHERE system_subject_id=$1
	UNION SELECT conversation_id FROM messages WHERE cardinality(mention_ids)>0 AND mention_ids @> ARRAY[$1]::bigint[]
) SELECT c.id FROM affected x JOIN conversations c USING(id) ORDER BY c.id FOR UPDATE OF c`

// Match the social package's canonical pair locks, including deleted
// friendships whose undo or request rows can still be changed.
const lockPurgeFriendPairs = `WITH peers(id) AS (
	SELECT user_high FROM friendships WHERE user_low=$1 UNION SELECT user_low FROM friendships WHERE user_high=$1
	UNION SELECT other_id FROM friend_preferences WHERE owner_id=$1 UNION SELECT owner_id FROM friend_preferences WHERE other_id=$1
	UNION SELECT recipient_id FROM friend_requests WHERE sender_id=$1 UNION SELECT sender_id FROM friend_requests WHERE recipient_id=$1
	UNION SELECT user_high FROM friend_deletions WHERE user_low=$1 UNION SELECT user_low FROM friend_deletions WHERE user_high=$1
) SELECT pg_advisory_xact_lock(hashtextextended('social:pair:'||least($1::bigint,id)::text||':'||greatest($1,id)::text,0))
FROM (SELECT id FROM peers ORDER BY id) ordered`

const orphanedAccountMedia = `NOT EXISTS(SELECT 1 FROM accounts a WHERE a.avatar_id=m.id AND a.id<>$1)
	AND NOT EXISTS(SELECT 1 FROM conversations c WHERE c.avatar_id=m.id AND c.archived_at IS NULL)
	AND NOT EXISTS(SELECT 1 FROM messages msg WHERE msg.attachment_id=m.id AND msg.recalled_at IS NULL)
	AND NOT EXISTS(SELECT 1 FROM stickers st WHERE st.media_id=m.id)
	AND NOT EXISTS(SELECT 1 FROM group_files gf WHERE gf.media_id=m.id)
	AND NOT EXISTS(SELECT 1 FROM space_post_media pm WHERE pm.media_id=m.id)`

var accountContentPurgeSteps = []struct{ name, sql string }{
	{"close conversations", `UPDATE conversations c SET archived_at=coalesce(archived_at,statement_timestamp()),
		title='',description='',announcement='',avatar_id=NULL,visibility='private',discoverable=false,
		invite_code=NULL,fixed_code=NULL,join_question='',allow_invite=false,join_policy='invite',
		global_mute=false,site_ban_until=NULL,site_ban_reason='',owner_transfer_cooldown_until=NULL
		WHERE c.id IN (` + closedAccountConversations + `)`},
	{"clear closed conversations' replay", `WITH closed AS MATERIALIZED (` + closedAccountConversations + `)
		DELETE FROM events e USING closed c WHERE e.conversation_id=c.id
		OR (e.conversation_id IS NULL AND e.data->>'conversation_id'=c.id::text)
		OR (e.kind='notice.created' AND e.data#>>'{message,conversation_id}'=c.id::text)`},
	{"clear message notices", `WITH deleted AS MATERIALIZED (` + deletedAccountMessages + `)
		DELETE FROM notifications n USING deleted d
		WHERE n.kind IN ('message.mention','message.reply') AND n.data?'conversation_id'
		AND n.data->>'conversation_id'=d.conversation_id::text AND n.data->>'seq'=d.seq::text`},
	{"clear message notification and action replay", `WITH deleted AS MATERIALIZED (` + deletedAccountMessages + `)
		DELETE FROM events e USING deleted d WHERE
		(e.kind='notice.created' AND e.data#>>'{message,conversation_id}'=d.conversation_id::text AND e.data#>>'{message,seq}'=d.seq::text)
		OR (e.kind IN ('message.recalled','message.featured') AND e.conversation_id=d.conversation_id AND e.data->>'seq'=d.seq::text)`},
	{"clear featured replay", `DELETE FROM events e USING featured_messages f WHERE f.added_by=$1
		AND e.conversation_id=f.conversation_id AND e.kind='message.featured' AND e.data->>'seq'=f.seq::text`},
	// Existing events carry a snapshot of structured fields. Remove references
	// from that snapshot before the message FK sets reply_seq to null.
	{"clear replay quote targets", `WITH deleted AS MATERIALIZED (` + deletedAccountMessages + `)
		UPDATE events e SET data=e.data-'reply_to'-'reply' FROM messages m JOIN deleted d
		ON (m.conversation_id,m.reply_seq)=(d.conversation_id,d.seq)
		WHERE (e.conversation_id,e.message_seq)=(m.conversation_id,m.seq) AND e.kind='message.created'`},
	{"clear replay mentions", `UPDATE events e SET data=jsonb_set(e.data,'{mention_ids}',to_jsonb(array_remove(m.mention_ids,$1::bigint)),false)
		FROM messages m WHERE (e.conversation_id,e.message_seq)=(m.conversation_id,m.seq) AND e.kind='message.created'
		AND cardinality(m.mention_ids)>0 AND m.mention_ids @> ARRAY[$1]::bigint[] AND e.data?'mention_ids'`},
	// Keep other users' retry digests reserved. Releasing a key would allow an
	// old write with that key to execute again after its resource was purged.
	// A read position is valid even when the message at that sequence is gone.
	{"clear retry result payloads", `WITH closed AS MATERIALIZED (` + closedAccountConversations + `),
		deleted AS MATERIALIZED (` + deletedAccountMessages + `)
		UPDATE command_keys k SET result='{"purged":true}'::jsonb WHERE k.user_id<>$1 AND k.result?'conversation_id'
		AND (EXISTS(SELECT 1 FROM closed c WHERE k.result->>'conversation_id'=c.id::text)
		OR (k.result ?| ARRAY['sender_id','recalled_at','featured'] AND EXISTS(SELECT 1 FROM deleted d
			WHERE k.result->>'conversation_id'=d.conversation_id::text AND k.result->>'seq'=d.seq::text)))`},
	{"clear feature and admission retry results", `WITH features AS (SELECT conversation_id,seq FROM featured_messages WHERE added_by=$1),
		admissions AS (SELECT id,conversation_id FROM group_join_requests WHERE user_id=$1 OR inviter_id=$1),
		transfers AS (SELECT id,conversation_id FROM group_transfers WHERE from_user_id=$1 OR to_user_id=$1)
		UPDATE command_keys k SET result='{"purged":true}'::jsonb WHERE k.user_id<>$1 AND k.result?'conversation_id'
		AND (k.result?'featured' AND EXISTS(SELECT 1 FROM features f
			WHERE k.result->>'conversation_id'=f.conversation_id::text AND k.result->>'seq'=f.seq::text)
		OR k.result?'request_id' AND EXISTS(SELECT 1 FROM admissions j
			WHERE k.result->>'conversation_id'=j.conversation_id::text AND k.result->>'request_id'=j.id::text)
		OR k.result?'transfer_id' AND EXISTS(SELECT 1 FROM transfers t
			WHERE k.result->>'conversation_id'=t.conversation_id::text AND k.result->>'transfer_id'=t.id::text))`},
	{"clear friend request retry results", `UPDATE command_keys k SET result='{"purged":true}'::jsonb
		FROM friend_requests r WHERE k.user_id<>$1 AND (r.sender_id=$1 OR r.recipient_id=$1)
		AND NOT (k.result?'conversation_id') AND k.result?'request_id' AND k.result->>'request_id'=r.id::text`},
	{"clear account retry keys", `DELETE FROM command_keys WHERE user_id=$1`},
	{"remove featured references", `WITH deleted AS MATERIALIZED (` + deletedAccountMessages + `)
		DELETE FROM featured_messages f WHERE f.added_by=$1 OR EXISTS (
		SELECT 1 FROM deleted d WHERE (f.conversation_id,f.seq)=(d.conversation_id,d.seq))`},
	{"remove messages", `DELETE FROM messages m USING (` + deletedAccountMessages + `) d
		WHERE (m.conversation_id,m.seq)=(d.conversation_id,d.seq)`},
	{"remove account-only replay", `DELETE FROM events e WHERE e.recipients @> ARRAY[$1]::bigint[]
		AND cardinality(array_remove(e.recipients,$1::bigint))=0`},
	{"remove account from replay audiences", `UPDATE events e SET recipients=array_remove(recipients,$1::bigint)
		WHERE e.recipients @> ARRAY[$1]::bigint[]`},
	{"remove relationship replay", `DELETE FROM events e WHERE e.kind IN ('friends.changed','groups.members_changed')
		AND e.data->>'user_id'=$1::bigint::text`},
	{"remove account references from surviving messages", `UPDATE messages m SET mention_ids=array_remove(mention_ids,$1::bigint),
		recalled_by=CASE WHEN recalled_by=$1 THEN NULL ELSE recalled_by END
		WHERE recalled_by=$1 OR (cardinality(mention_ids)>0 AND mention_ids @> ARRAY[$1]::bigint[])`},
	{"remove group and friend notices", `WITH closed AS MATERIALIZED (` + closedAccountConversations + `),
		admissions AS (SELECT id FROM group_join_requests WHERE user_id=$1 OR inviter_id=$1),
		transfers AS (SELECT id FROM group_transfers WHERE from_user_id=$1 OR to_user_id=$1),
		deletions AS (SELECT notification_id FROM friend_deletions WHERE user_low=$1 OR user_high=$1)
		DELETE FROM notifications n WHERE n.user_id=$1
		OR (n.data?'user_id' AND n.data->>'user_id'=$1::bigint::text)
		OR (n.kind='legacy.private_system' AND n.data->>'from_user_id'=$1::bigint::text)
		OR (n.kind='friend.deleted' AND n.data->>'deleted_by'=$1::bigint::text)
		OR (n.data?'conversation_id' AND EXISTS(SELECT 1 FROM closed c WHERE n.data->>'conversation_id'=c.id::text))
		OR (n.kind IN ('group.invited','group.joined','group.join_responded','group.invitation_responded')
			AND EXISTS(SELECT 1 FROM admissions j WHERE n.data->>'request_id'=j.id::text))
		OR (n.kind IN ('group.transfer_requested','group.transfer_responded')
			AND EXISTS(SELECT 1 FROM transfers t WHERE n.data->>'transfer_id'=t.id::text))
		OR n.id IN (SELECT notification_id FROM deletions)`},
	{"remove memberships", `DELETE FROM conversation_members WHERE user_id=$1 OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove admission requests", `DELETE FROM group_join_requests WHERE user_id=$1 OR inviter_id=$1
		OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove transfer requests", `DELETE FROM group_transfers WHERE from_user_id=$1 OR to_user_id=$1
		OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove group files", `DELETE FROM group_files WHERE uploader_id=$1 OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove closed group folders", `DELETE FROM group_file_folders WHERE conversation_id IN (` + closedAccountConversations + `)`},
	{"remove friendship requests", `DELETE FROM friend_requests WHERE sender_id=$1 OR recipient_id=$1`},
	{"remove friendships", `DELETE FROM friendships WHERE user_low=$1 OR user_high=$1`},
	{"remove friendship preferences", `DELETE FROM friend_preferences WHERE owner_id=$1 OR other_id=$1`},
	// Match space.remove by locking thread roots before removing any replies.
	{"lock affected space roots", `SELECT root.id FROM space_posts root
		WHERE root.parent_id IS NULL AND root.id IN (
			SELECT coalesce(parent_id,id) FROM space_posts WHERE author_id=$1
		) ORDER BY root.id FOR UPDATE OF root`},
	// Replies and likes belonging to a removed root have no independent space
	// audience. Their existing cascades do not affect other surviving threads.
	{"remove space entries", `DELETE FROM space_posts WHERE author_id=$1`},
	{"remove space reactions", `DELETE FROM space_likes WHERE user_id=$1`},
	{"remove membership activity data", `DELETE FROM group_member_active_days WHERE user_id=$1 OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove membership experience data", `DELETE FROM group_member_experience WHERE user_id=$1 OR conversation_id IN (` + closedAccountConversations + `)`},
	{"remove activity", `DELETE FROM user_activity WHERE user_id=$1`},
	// Keep the moderation record and other users' ability to review the case.
	{"detach report identities", `UPDATE reports SET reporter_id=CASE WHEN reporter_id=$1 THEN NULL ELSE reporter_id END,
		reviewer_id=CASE WHEN reviewer_id=$1 THEN NULL ELSE reviewer_id END WHERE reporter_id=$1 OR reviewer_id=$1`},
	{"remove sessions", `DELETE FROM account_sessions WHERE user_id=$1`},
	{"remove linked challenges", `DELETE FROM account_email_challenges WHERE subject_id=$1`},
}
