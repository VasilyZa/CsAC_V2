package chat

import (
	"context"
	"fmt"
	"strings"

	"csac/v2/internal/rpc"
)

func renderNudge(ctx context.Context, db rpc.DB, senderID, targetID int64) (string, error) {
	var sender, action, target string
	err := db.QueryRow(ctx, `SELECT sender.nickname,sender.pat_action,target.nickname
		FROM accounts sender CROSS JOIN accounts target WHERE sender.id=$1 AND target.id=$2`, senderID, targetID).
		Scan(&sender, &action, &target)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(sender) == "" {
		sender = fmt.Sprintf("UID %d", senderID)
	}
	if strings.TrimSpace(target) == "" {
		target = fmt.Sprintf("UID %d", targetID)
	}
	if strings.TrimSpace(action) == "" {
		action = "拍了拍"
	}
	return sender + action + target, nil
}
