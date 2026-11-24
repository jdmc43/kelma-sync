package handler

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// applyReviewNoteDeleteTx removes only canonical note content. Card identity,
// projections, and immutable review history remain available for audit, while the
// note tombstone makes every client remove the note and its sibling cards.
func applyReviewNoteDeleteTx(ctx context.Context, tx pgx.Tx, userID, clientID, noteGUID string, now time.Time) error {
	result, err := tx.Exec(ctx, `DELETE FROM notes WHERE user_id=$1 AND guid=$2`, userID, noteGUID)
	if err != nil || result.RowsAffected() != 1 {
		return errors.New("review note delete failed")
	}
	_, err = tx.Exec(ctx, `INSERT INTO tombstones
		(user_id,type,resource_id,deleted_at,last_client_id,note_guid,card_ord)
		VALUES($1,'note',$2,$3,$4::uuid,NULL,NULL)
		ON CONFLICT(user_id,type,resource_id) DO UPDATE SET
		deleted_at=EXCLUDED.deleted_at,last_client_id=EXCLUDED.last_client_id,
		note_guid=NULL,card_ord=NULL`, userID, noteGUID, now.UTC(), clientID)
	if err != nil {
		return errors.New("review note tombstone failed")
	}
	return nil
}
