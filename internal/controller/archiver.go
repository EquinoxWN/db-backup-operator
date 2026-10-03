package controller

import (
	"fmt"
	"strconv"
	"strings"

	backupv1 "github.com/EquinoxWN/db-backup-operator/api/v1alpha1"
)

// archiverQuery reads PostgreSQL's WAL archiver statistics as one '|'-separated row.
const archiverQuery = "SELECT archived_count, failed_count, coalesce(last_archived_wal, ''), " +
	"coalesce(last_archived_time::text, ''), coalesce(last_failed_time::text, '') FROM pg_stat_archiver"

// parseArchiver turns psql -At output into a WALArchiveStatus.
func parseArchiver(out string) (*backupv1.WALArchiveStatus, error) {
	fields := strings.Split(strings.TrimSpace(out), "|")
	if len(fields) != 5 {
		return nil, fmt.Errorf("unexpected pg_stat_archiver output %q", strings.TrimSpace(out))
	}
	archived, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("archived_count: %w", err)
	}
	failed, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("failed_count: %w", err)
	}
	return &backupv1.WALArchiveStatus{
		ArchivedCount:    archived,
		FailedCount:      failed,
		LastArchivedWAL:  fields[2],
		LastArchivedTime: fields[3],
		LastFailedTime:   fields[4],
	}, nil
}

// archivingHealthy reports whether WAL is being archived and the latest attempt did not fail.
func archivingHealthy(s *backupv1.WALArchiveStatus) (bool, string) {
	switch {
	case s.ArchivedCount == 0:
		return false, "no WAL segment has been archived yet; check archive_mode and archive_command"
	case s.LastFailedTime != "" && s.LastFailedTime >= s.LastArchivedTime:
		return false, fmt.Sprintf("the most recent archive attempt failed at %s", s.LastFailedTime)
	default:
		return true, fmt.Sprintf("%d segments archived, last %s at %s", s.ArchivedCount, s.LastArchivedWAL, s.LastArchivedTime)
	}
}
