package storage

import (
	"context"
	"testing"
	"time"
)

func TestFailoverAncestorJournalExpiresWithoutLosingCurrentHead(t *testing.T) {
	d, m := failoverPair(t)
	ctx := context.Background()
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('ancestor-data','kept')")
	if err := d.catchUp(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := position(ctx, d.source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.freeze(ctx, "active_unreachable"); err != nil {
		t.Fatal(err)
	}
	if err = d.switchTo(ctx, "standby", original); err != nil {
		t.Fatal(err)
	}
	replicaWrite(t, d.source, "INSERT INTO settings VALUES('new-epoch-data','kept')")
	if err = d.synchronize(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := position(ctx, d.source.current.reader)
	if err != nil {
		t.Fatal(err)
	}
	var before int
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", original.Epoch).Scan(&before); err != nil || before == 0 {
		t.Fatal("fixture lost its real ancestor transaction", before, err)
	}
	if err = d.prune(ctx, time.Now().Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var ancestors, headers, rows int
	var bytes, sum int64
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=?", original.Epoch).Scan(&ancestors); err != nil {
		t.Fatal(err)
	}
	if ancestors != 0 {
		t.Error("expired canonical ancestor journal survived a generation switch", ancestors)
	}
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM replica_transactions WHERE epoch_uuid=? AND seq=? AND tx_uuid=?", head.Epoch, head.Seq, head.ID).Scan(&headers); err != nil || headers != 1 {
		t.Fatal("GC removed the current receipt/proof boundary", headers, err)
	}
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT journal_bytes,(SELECT COALESCE(SUM(payload_bytes),0) FROM replica_transactions) FROM replica_state WHERE id=1").Scan(&bytes, &sum); err != nil || bytes != sum {
		t.Fatal("ancestor accounting diverged", bytes, sum, err)
	}
	if err = d.source.current.reader.QueryRowContext(ctx, "SELECT count(*) FROM settings").Scan(&rows); err != nil || rows != 2 || !m.Ready {
		t.Fatal("ancestor expiry changed business rows or replica eligibility", rows, m.Ready, err)
	}
}
