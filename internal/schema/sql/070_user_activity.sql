CREATE TABLE user_activity (
    user_id bigint PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
    last_active timestamptz NOT NULL
);

INSERT INTO user_activity(user_id,last_active)
SELECT user_id,max(created_at) FROM account_sessions GROUP BY user_id;
