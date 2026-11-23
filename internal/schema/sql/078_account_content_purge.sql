-- Legacy imports and earlier maintenance already use purged_at for the
-- account row. A separate completion marker also queues those tombstones.
ALTER TABLE accounts
    ADD COLUMN content_purged_at timestamptz,
    ADD COLUMN content_purge_retry_at timestamptz,
    ADD CHECK (content_purged_at IS NULL OR (status='deleted' AND purged_at IS NOT NULL));
CREATE INDEX accounts_content_purge_queue ON accounts (coalesce(deleted_at,purged_at), id)
    WHERE status='deleted' AND content_purged_at IS NULL;

-- Keep identifiers of surviving messages intact when a quote target disappears.
ALTER TABLE messages DROP CONSTRAINT messages_conversation_id_reply_seq_fkey,
    ADD CONSTRAINT messages_conversation_id_reply_seq_fkey
        FOREIGN KEY (conversation_id,reply_seq) REFERENCES messages(conversation_id,seq) ON DELETE SET NULL (reply_seq);
CREATE INDEX messages_reply_fk ON messages(conversation_id,reply_seq) WHERE reply_seq IS NOT NULL;
CREATE INDEX messages_purge_sender ON messages(sender_id,conversation_id,seq);
CREATE INDEX messages_purge_nudge ON messages(nudge_target_id,conversation_id,seq) WHERE nudge_target_id IS NOT NULL;
CREATE INDEX messages_purge_recaller ON messages(recalled_by,conversation_id) WHERE recalled_by IS NOT NULL;
-- The inbox still filters recalled rows. The purge also clears their mentions.
DROP INDEX messages_attention_mentions;
CREATE INDEX messages_attention_mentions ON messages USING gin(mention_ids) WHERE cardinality(mention_ids)>0;

-- Message-scoped event payloads are owned by their message. Other retained
-- event shapes are handled explicitly by the sweep.
ALTER TABLE events DROP CONSTRAINT events_message_scope_fk,
    ADD CONSTRAINT events_message_scope_fk
        FOREIGN KEY (conversation_id,message_seq) REFERENCES messages(conversation_id,seq) ON DELETE CASCADE;
CREATE INDEX events_conversation_message ON events(conversation_id,message_seq) WHERE conversation_id IS NOT NULL;
CREATE INDEX events_unscoped_conversation ON events((data->>'conversation_id')) WHERE conversation_id IS NULL;
CREATE INDEX events_message_action ON events(conversation_id,(data->>'seq')) WHERE kind IN ('message.recalled','message.featured');
CREATE INDEX events_notice_message ON events((data#>>'{message,conversation_id}'),(data#>>'{message,seq}')) WHERE kind='notice.created';

CREATE INDEX conversations_purge_owner ON conversations(owner_id,id) WHERE kind='group';
CREATE INDEX conversations_direct_high ON conversations(direct_high,id) WHERE kind='direct';
CREATE INDEX friend_preferences_other ON friend_preferences(other_id,owner_id);
CREATE INDEX friend_deletions_high ON friend_deletions(user_high,user_low);
CREATE INDEX group_join_requests_inviter ON group_join_requests(inviter_id,conversation_id) WHERE inviter_id IS NOT NULL;
CREATE INDEX group_transfers_by_conversation ON group_transfers(conversation_id);
CREATE INDEX group_files_uploader ON group_files(uploader_id,conversation_id);
CREATE INDEX featured_messages_added_by ON featured_messages(added_by,conversation_id);
CREATE INDEX media_objects_owner ON media_objects(owner_id,id);
CREATE INDEX space_posts_purge_author ON space_posts(author_id,id);
CREATE INDEX space_likes_user ON space_likes(user_id,post_id);
CREATE INDEX account_email_challenges_all_subject ON account_email_challenges(subject_id) WHERE subject_id IS NOT NULL;
CREATE INDEX notifications_conversation_message ON notifications((data->>'conversation_id'),(data->>'seq'))
    WHERE data?'conversation_id';
CREATE INDEX notifications_purge_actor ON notifications((data->>'user_id')) WHERE data?'user_id';
CREATE INDEX notifications_purge_transfer ON notifications((data->>'transfer_id')) WHERE kind IN ('group.transfer_requested','group.transfer_responded');
CREATE INDEX notifications_purge_admission ON notifications((data->>'request_id'))
    WHERE kind IN ('group.invited','group.joined','group.join_responded','group.invitation_responded');
CREATE INDEX stickers_media_reference ON stickers(media_id) WHERE media_id IS NOT NULL;
CREATE INDEX command_keys_conversation_result ON command_keys((result->>'conversation_id'),(result->>'seq'))
    WHERE result?'conversation_id';
CREATE INDEX events_purge_subject ON events((data->>'user_id')) WHERE kind IN ('friends.changed','groups.members_changed');
CREATE INDEX command_keys_friend_request_result ON command_keys((result->>'request_id'))
    WHERE result?'request_id' AND NOT (result?'conversation_id');
CREATE INDEX group_member_active_days_user ON group_member_active_days(user_id,conversation_id);
CREATE INDEX group_member_experience_user ON group_member_experience(user_id,conversation_id);
CREATE INDEX reports_purge_reviewer ON reports(reviewer_id) WHERE reviewer_id IS NOT NULL;

-- A buffered presence flush may arrive after the purge. Wait for an in-flight
-- account purge before deciding whether this last-active write still applies.
CREATE FUNCTION retain_activity_for_existing_identity() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM accounts WHERE id=NEW.user_id AND content_purged_at IS NULL FOR KEY SHARE;
    IF FOUND THEN
        RETURN NEW;
    END IF;
    RETURN NULL;
END;
$$;
CREATE TRIGGER user_activity_live_identity BEFORE INSERT ON user_activity FOR EACH ROW
    EXECUTE FUNCTION retain_activity_for_existing_identity();
