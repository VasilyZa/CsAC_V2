package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5"
)

type pendingEvent struct {
	kind           string
	recipients     []int64
	data           json.RawMessage
	conversationID *int64
	messageSeq     *int64
}
type pendingEvents []pendingEvent

func (p *pendingEvents) add(kind string, recipients []int64, value any) error {
	return p.addEvent(nil, nil, kind, recipients, value)
}

func (p *pendingEvents) addChat(conversationID, messageSeq int64, kind string, recipients []int64, value any) error {
	if conversationID <= 0 || messageSeq < 0 {
		return errors.New("invalid event scope")
	}
	var seq *int64
	if messageSeq != 0 {
		seq = &messageSeq
	}
	return p.addEvent(&conversationID, seq, kind, recipients, value)
}

func (p *pendingEvents) addEvent(conversationID, messageSeq *int64, kind string, recipients []int64, value any) error {
	if len(recipients) == 0 {
		return nil
	}
	if len(*p) >= 100 || len(recipients) > 20000 {
		return errors.New("event fanout exceeds bounds")
	}
	if !operationName.MatchString(kind) {
		return errors.New("invalid event kind")
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 64<<10 {
		return errors.New("event exceeds size limit")
	}
	ids := slices.Clone(recipients)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	ids = slices.Compact(ids)
	if ids[0] <= 0 {
		return errors.New("invalid recipient")
	}
	*p = append(*p, pendingEvent{kind, ids, data, conversationID, messageSeq})
	return nil
}

func (p pendingEvents) appendTo(ctx context.Context, tx pgx.Tx) error {
	if len(p) == 0 {
		return nil
	}
	var head int64
	if err := tx.QueryRow(ctx, `UPDATE event_clock SET head=head+$1 WHERE singleton RETURNING head`, int64(len(p))).Scan(&head); err != nil {
		return err
	}
	batch := &pgx.Batch{}
	for i, event := range p {
		batch.Queue(`INSERT INTO events(seq,kind,recipients,data,conversation_id,message_seq) VALUES ($1,$2,$3,$4,$5,$6)`, head-int64(len(p)-i)+1, event.kind, event.recipients, event.data, event.conversationID, event.messageSeq)
	}
	batch.Queue(`SELECT pg_notify('csac_events','')`)
	return tx.SendBatch(ctx, batch).Close()
}
