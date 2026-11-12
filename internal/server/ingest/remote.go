package ingest

import (
	"context"
	"net/netip"
	"time"

	"orbitbeacon/internal/server/storage"
)

// ObserveRemote shares the per-node state; stable IPs never issue a write or read.
// Metadata eligibility is refreshed by the report's existing identity query.
func (s *Service) ObserveRemote(ctx context.Context, id, ip string) (bool, bool, error) {
	addr, err := netip.ParseAddr(ip)
	if err != nil || addr.Zone() != "" {
		return false, false, nil
	}
	ip = addr.Unmap().String()
	state := s.acquireState(id)
	defer s.releaseState(state)
	state.remoteMu.Lock()
	defer state.remoteMu.Unlock()
	changed := false
	if state.remoteIP != ip {
		lease, err := s.db.Acquire(ctx, storage.Writer)
		if err != nil {
			return false, false, err
		}
		defer lease.Close()
		tx, err := lease.BeginWrite(lease.Context())
		if err != nil {
			return false, false, err
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(lease.Context(), "UPDATE nodes SET remote_ip=?,geo_checked_at='',geo_country='',geo_location='' WHERE id=? AND remote_ip<>?", ip, id, ip)
		if err != nil {
			return false, false, err
		}
		if err := tx.Commit(); err != nil {
			return false, false, err
		}
		n, _ := result.RowsAffected()
		changed = n > 0
		state.remoteIP = ip
		state.geoChecked = ""
	}
	checked, err := time.Parse(time.RFC3339Nano, state.geoChecked)
	return state.remoteRegion == "" && (err != nil || s.now().Sub(checked) >= 24*time.Hour), changed, nil
}
