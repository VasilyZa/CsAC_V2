-- Index the active messages that can enter the attention inbox.
CREATE INDEX messages_attention_mentions ON messages USING GIN (mention_ids)
    WHERE recalled_at IS NULL AND cardinality(mention_ids) > 0;

CREATE INDEX messages_attention_replies ON messages (conversation_id, reply_seq, seq)
    WHERE recalled_at IS NULL AND reply_seq IS NOT NULL;
