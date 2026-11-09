package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"

	"github.com/google/uuid"
)

func validateProtocolState(ctx context.Context, db *Database, proof replicaProof) error {
	tx, err := db.current.writer.BeginTx(ctx, nil)
	if err != nil {
		return safeFailure(err)
	}
	defer tx.Rollback()
	guard := "SELECT last_tx_uuid FROM storage_write_guard WHERE id=1"
	if db.current.dialect == MySQL {
		guard += " FOR UPDATE"
	}
	var receipt string
	if err = tx.QueryRowContext(ctx, guard).Scan(&receipt); err != nil {
		return safeFailure(err)
	}
	var identities, states int
	if err = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM replica_identity),(SELECT count(*) FROM replica_state)").Scan(&identities, &states); err != nil {
		return safeFailure(err)
	}
	if identities != 1 || states != 1 {
		return ErrIdentityMismatch
	}
	var p replicaPosition
	var retained, ordinal, owner, journal, ready int64
	var mode, open string
	if err = tx.QueryRowContext(ctx, "SELECT epoch_uuid,head_seq,head_tx_uuid,retained_from_seq,capture_mode,open_tx_uuid,next_ordinal,owner_connection,journal_bytes,ready FROM replica_state WHERE id=1").Scan(&p.Epoch, &p.Seq, &p.ID, &retained, &mode, &open, &ordinal, &owner, &journal, &ready); err != nil {
		return ErrIdentityMismatch
	}
	if p.Epoch == "" || p.Seq < 0 || p.Seq == math.MaxInt64 || retained < 1 || retained > p.Seq+1 || journal < 0 || (ready != 0 && ready != 1) || (mode != "off" && mode != "source") || open != "" || ordinal != 0 || owner != 0 || (proof.Ready && ready != 1) {
		return ErrIdentityMismatch
	}
	var parent, parentID string
	var parentSeq int64
	if err = tx.QueryRowContext(ctx, "SELECT parent_epoch_uuid,parent_seq,parent_tx_uuid FROM replica_epochs WHERE epoch_uuid=?", p.Epoch).Scan(&parent, &parentSeq, &parentID); err != nil {
		return ErrIdentityMismatch
	}
	if parent == "" {
		if parentSeq != 0 || parentID != "" {
			return ErrIdentityMismatch
		}
	} else {
		var count int
		if parent == p.Epoch || parentSeq < 0 || (parentSeq == 0 && parentID != "") || (parentSeq > 0 && !replicaUUID(parentID)) {
			return ErrIdentityMismatch
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM replica_epochs WHERE epoch_uuid=?", parent).Scan(&count); err != nil {
			return safeFailure(err)
		}
		if count != 1 {
			return ErrIdentityMismatch
		}
	}
	var sum int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions").Scan(&sum); err != nil {
		return safeFailure(err)
	}
	if sum != journal {
		return ErrIdentityMismatch
	}
	if p.Seq == 0 {
		var count int
		if p.ID != "" || retained != 1 {
			return ErrIdentityMismatch
		}
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", p.Epoch).Scan(&count); err != nil {
			return safeFailure(err)
		}
		if count != 0 {
			return ErrIdentityMismatch
		}
		return nil
	}
	if !replicaUUID(p.ID) {
		return ErrIdentityMismatch
	}
	var id, previous, boot string
	var barrier, count, bytes int64
	err = tx.QueryRowContext(ctx, "SELECT tx_uuid,previous_tx_uuid,boot_id,barrier_ns,change_count,payload_bytes FROM replica_transactions WHERE epoch_uuid=? AND seq=?", p.Epoch, p.Seq).Scan(&id, &previous, &boot, &barrier, &count, &bytes)
	if errors.Is(err, sql.ErrNoRows) {
		// seed 头不是源事务日志；只有控制记录证明的完整快照或待重做的 inactive 操作能采用该形态。
		checkpoint := proof.Checkpoint != nil && *proof.Checkpoint == p
		if mode != "off" || retained != p.Seq+1 || journal != 0 || (!checkpoint && !proof.Rebuilding) {
			return ErrIdentityMismatch
		}
		return nil
	}
	if err != nil {
		return safeFailure(err)
	}
	if id != p.ID || !replicaUUID(boot) || barrier < 0 || count < 0 || bytes < transactionOverhead || bytes > maxTransactionBytes || retained > p.Seq || (p.Seq == 1 && previous != "") || (p.Seq > 1 && !replicaUUID(previous)) {
		return ErrIdentityMismatch
	}
	length := "COALESCE(SUM(length(CAST(primary_key AS BLOB))+COALESCE(length(CAST(after_image AS BLOB)),0)+64),0)"
	if db.current.dialect == MySQL {
		length = "COALESCE(SUM(OCTET_LENGTH(primary_key)+COALESCE(OCTET_LENGTH(after_image),0)+64),0)"
	}
	var actual, min, max, payload int64
	if err = tx.QueryRowContext(ctx, "SELECT count(*),COALESCE(MIN(ordinal),0),COALESCE(MAX(ordinal),-1),"+length+" FROM replica_changes WHERE epoch_uuid=? AND seq=?", p.Epoch, p.Seq).Scan(&actual, &min, &max, &payload); err != nil {
		return safeFailure(err)
	}
	if actual != count || min != 0 || max != count-1 || payload+transactionOverhead != bytes {
		return ErrIdentityMismatch
	}
	return nil
}
func replicaUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed.String() == value
}
