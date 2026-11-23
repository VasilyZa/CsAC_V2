package rpc

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

type commandStatus struct {
	State  string          `json:"state"`
	Result json.RawMessage `json:"result,omitempty"`
}

func readCommandStatus(ctx context.Context, call Call, raw json.RawMessage) (any, error) {
	in, err := Decode[struct {
		Key string `json:"key"`
	}](raw)
	if err != nil {
		return commandStatus{}, err
	}
	if !retryKey.MatchString(in.Key) {
		return commandStatus{}, Invalid("Invalid retry key")
	}
	status := commandStatus{State: "unknown"}
	err = call.DB.QueryRow(ctx, `SELECT result FROM command_keys WHERE user_id=$1 AND key=$2`, call.Actor.ID, in.Key).Scan(&status.Result)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, nil
	}
	if err == nil && status.Result != nil {
		status.State = "complete"
	}
	return status, err
}
