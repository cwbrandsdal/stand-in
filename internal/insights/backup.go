package insights

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

// Nightly backups copy the database with VACUUM INTO, which writes a consistent, compacted snapshot
// while the hub keeps recording. INSIGHTS_BACKUP_DIR enables them.

const (
	defaultBackupKeep   = 14
	defaultBackupHour   = 3
	backupFirstCheck    = 2 * time.Minute
	backupCheckInterval = 10 * time.Minute
	backupFilePrefix    = "insights-"
	backupFileSuffix    = ".db"
	metaBackupLastMs    = "backup_last_ms"
	metaBackupLastFile  = "backup_last_file"
	metaBackupLastBytes = "backup_last_bytes"
	metaBackupLastError = "backup_last_error"
)

// BackupStatus describes the backup configuration and the latest run.
type BackupStatus struct {
	Enabled   bool   `json:"enabled"`
	Dir       string `json:"dir,omitempty"`
	Keep      int    `json:"keep"`
	Hour      int    `json:"hour"`
	LastMs    int64  `json:"last_ms,omitempty"`
	LastFile  string `json:"last_file,omitempty"`
	LastBytes int64  `json:"last_bytes,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

func backupDir() string {
	dir := strings.TrimSpace(os.Getenv("INSIGHTS_BACKUP_DIR"))
	if dir == "" {
		return ""
	}
	if absolute, errAbs := filepath.Abs(dir); errAbs == nil {
		return absolute
	}
	return filepath.Clean(dir)
}

func backupKeep() int { return envInt("INSIGHTS_BACKUP_KEEP", defaultBackupKeep) }

// backupHour is the local hour after which the daily backup runs.
func backupHour() int {
	if hour, errParse := strconv.Atoi(strings.TrimSpace(os.Getenv("INSIGHTS_BACKUP_HOUR"))); errParse == nil && hour >= 0 && hour <= 23 {
		return hour
	}
	return defaultBackupHour
}

// backupDue reports whether the latest scheduled backup time (today or yesterday at hour, local
// time) is after the last backup.
func backupDue(last, now time.Time, hour int) bool {
	local := now.In(time.Local)
	scheduled := time.Date(local.Year(), local.Month(), local.Day(), hour, 0, 0, 0, time.Local)
	if local.Before(scheduled) {
		scheduled = scheduled.AddDate(0, 0, -1)
	}
	return last.Before(scheduled)
}

// backupTo writes a snapshot of the database into dir and returns its path.
func (s *Store) backupTo(ctx context.Context, dir string, now time.Time) (string, error) {
	if errMkdir := os.MkdirAll(dir, 0o700); errMkdir != nil {
		return "", fmt.Errorf("insights: create backup directory: %w", errMkdir)
	}
	local := now.In(time.Local)
	path := filepath.Join(dir, backupFilePrefix+local.Format("20060102")+backupFileSuffix)
	if _, errStat := os.Stat(path); errStat == nil {
		path = filepath.Join(dir, backupFilePrefix+local.Format("20060102-150405")+backupFileSuffix)
	}
	partial := path + ".partial"
	if errRemove := os.Remove(partial); errRemove != nil && !os.IsNotExist(errRemove) {
		return "", fmt.Errorf("insights: remove stale partial backup: %w", errRemove)
	}
	if _, errExec := s.db.ExecContext(ctx, `VACUUM INTO ?`, partial); errExec != nil {
		_ = os.Remove(partial)
		return "", fmt.Errorf("insights: write backup: %w", errExec)
	}
	if errRename := os.Rename(partial, path); errRename != nil {
		_ = os.Remove(partial)
		return "", fmt.Errorf("insights: finish backup: %w", errRename)
	}
	return path, nil
}

// pruneBackups keeps the newest keep snapshots in dir. Without the suffix, names sort by date and a
// same-day extra copy (insights-YYYYMMDD-HHMMSS) after the daily one, so the newest is last.
func pruneBackups(dir string, keep int) error {
	entries, errRead := os.ReadDir(dir)
	if errRead != nil {
		return fmt.Errorf("insights: list backups: %w", errRead)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && strings.HasPrefix(name, backupFilePrefix) && strings.HasSuffix(name, backupFileSuffix) {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.TrimSuffix(names[i], backupFileSuffix) < strings.TrimSuffix(names[j], backupFileSuffix)
	})
	for len(names) > keep {
		if errRemove := os.Remove(filepath.Join(dir, names[0])); errRemove != nil {
			return fmt.Errorf("insights: remove old backup: %w", errRemove)
		}
		names = names[1:]
	}
	return nil
}

// backupNow writes a snapshot, prunes old ones and records the result.
func (s *Service) backupNow(ctx context.Context, now time.Time) (string, error) {
	dir := backupDir()
	if dir == "" {
		return "", fmt.Errorf("backups are off: set INSIGHTS_BACKUP_DIR")
	}
	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	path, errBackup := s.store.backupTo(ctx, dir, now)
	if errBackup == nil {
		errBackup = pruneBackups(dir, backupKeep())
	}
	message := ""
	if errBackup != nil {
		message = errBackup.Error()
	}
	values := map[string]string{metaBackupLastError: message}
	if path != "" {
		values[metaBackupLastMs] = strconv.FormatInt(now.UnixMilli(), 10)
		values[metaBackupLastFile] = filepath.Base(path)
		if info, errStat := os.Stat(path); errStat == nil {
			values[metaBackupLastBytes] = strconv.FormatInt(info.Size(), 10)
		}
	}
	for key, value := range values {
		if errMeta := s.store.setMetaValue(ctx, key, value); errMeta != nil {
			log.Warnf("insights: %v", errMeta)
		}
	}
	return path, errBackup
}

func (s *Service) backupStatus(ctx context.Context) BackupStatus {
	status := BackupStatus{Dir: backupDir(), Keep: backupKeep(), Hour: backupHour()}
	status.Enabled = status.Dir != ""
	read := func(key string) string {
		value, errRead := s.store.metaValue(ctx, key)
		if errRead != nil {
			log.Warnf("insights: %v", errRead)
		}
		return value
	}
	status.LastMs, _ = strconv.ParseInt(read(metaBackupLastMs), 10, 64)
	status.LastBytes, _ = strconv.ParseInt(read(metaBackupLastBytes), 10, 64)
	status.LastFile = read(metaBackupLastFile)
	status.LastError = read(metaBackupLastError)
	return status
}

func (s *Service) runBackups(ctx context.Context) {
	if backupDir() == "" {
		return
	}
	timer := time.NewTimer(backupFirstCheck)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now().UTC()
		status := s.backupStatus(ctx)
		if backupDue(time.UnixMilli(status.LastMs), now, status.Hour) {
			if path, errBackup := s.backupNow(ctx, now); errBackup != nil {
				if ctx.Err() != nil {
					return
				}
				log.Warnf("insights: backup failed: %v", errBackup)
			} else {
				log.Infof("insights: backed up database to %s", path)
			}
		}
		timer.Reset(backupCheckInterval)
	}
}
