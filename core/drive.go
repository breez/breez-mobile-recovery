package core

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/option"
)

// Names the Breez app uses in Drive's hidden app folder (breez library,
// backup/drive.go): one "snapshot-<node id>" folder per node, whose
// properties point at the folder holding the newest backup files.
const (
	driveSnapshotPrefix      = "snapshot-"
	driveActiveFolderProp    = "activeBackupFolder"
	driveBackupIDProp        = "backupID"
	driveLegacyEncryptedProp = "backupEncrypted"
	driveEncryptionTypeProp  = "backupEncryptionType"
	driveBackupTimeProp      = "backupModifiedTimestamp"
)

// listDriveSnapshots lists the backups in the account's Breez app folder.
// It only reads. The breez library has a listing of its own, but it needs
// the library initialised on a folder before the user has chosen a backup,
// and the library stays bound to its first folder for the whole process.
//
// The time shown for a backup is when its files were written. The snapshot
// folder's own time is not used: restoring a backup updates the folder and
// would make an old backup look new.
func listDriveSnapshots(ctx context.Context, auth *googleAuth) ([]Snapshot, error) {
	svc, err := drive.NewService(ctx, option.WithTokenSource(auth.src))
	if err != nil {
		return nil, err
	}
	var out []Snapshot
	pageToken := ""
	for {
		call := svc.Files.List().Spaces("appDataFolder").Context(ctx).
			Fields("nextPageToken", "files(id,name,modifiedTime,appProperties)").
			Q("'appDataFolder' in parents and name contains '" + driveSnapshotPrefix + "'").
			PageSize(1000)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		r, err := call.Do()
		if err != nil {
			return nil, fmt.Errorf("list the backups in Google Drive: %w", err)
		}
		for _, f := range r.Files {
			if !strings.HasPrefix(f.Name, driveSnapshotPrefix) {
				continue
			}
			snap := Snapshot{
				NodeID:         strings.TrimPrefix(f.Name, driveSnapshotPrefix),
				BackupID:       f.AppProperties[driveBackupIDProp],
				EncryptionType: f.AppProperties[driveEncryptionTypeProp],
			}
			snap.Encrypted = snap.EncryptionType != "" || f.AppProperties[driveLegacyEncryptedProp] == "true"
			if snap.Encrypted && snap.EncryptionType == "" {
				snap.EncryptionType = "PIN"
			}
			if t, err := time.Parse(time.RFC3339, f.AppProperties[driveBackupTimeProp]); err == nil {
				snap.ModifiedTime = t
			} else if t, err := driveBackupFilesTime(ctx, svc, f.AppProperties[driveActiveFolderProp]); err == nil && !t.IsZero() {
				snap.ModifiedTime = t
			} else if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil {
				snap.ModifiedTime = t
			}
			out = append(out, snap)
		}
		if pageToken = r.NextPageToken; pageToken == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedTime.After(out[j].ModifiedTime) })
	return out, nil
}

// driveBackupFilesTime is when the node files of a backup were last written.
func driveBackupFilesTime(ctx context.Context, svc *drive.Service, folderID string) (time.Time, error) {
	var newest time.Time
	if folderID == "" {
		return newest, nil
	}
	r, err := svc.Files.List().Spaces("appDataFolder").Context(ctx).
		Fields("files(name,modifiedTime)").
		Q(fmt.Sprintf("'%s' in parents", folderID)).Do()
	if err != nil {
		return newest, err
	}
	for _, f := range r.Files {
		// The app data zip (podcasts and the like) is written on its own
		// schedule and says nothing about the node's state.
		if f.Name == "app_data_backup.zip" {
			continue
		}
		if t, err := time.Parse(time.RFC3339, f.ModifiedTime); err == nil && t.After(newest) {
			newest = t
		}
	}
	return newest, nil
}
