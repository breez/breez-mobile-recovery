package core

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
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

// driveBackup is a backup fetched from Drive, not yet placed.
type driveBackup struct {
	files map[string][]byte // what the backup folder holds, by file name
	svc   *drive.Service
	node  string // id of the node's snapshot folder
}

// downloadDriveBackup fetches the files of the node's newest backup into
// memory. It only reads. (The library's own restore is not used: its
// RestoreBackup returns the result of an unrelated call and so reports a
// failed restore as done, bindings/api.go:317-328; it marks the snapshot as
// restored before it has decrypted anything; and it downloads through the
// system's temp folder, which fails across drives on Windows.)
func downloadDriveBackup(ctx context.Context, auth *googleAuth, nodeID string) (*driveBackup, error) {
	svc, err := drive.NewService(ctx, option.WithTokenSource(auth.src))
	if err != nil {
		return nil, err
	}
	folders, err := driveList(ctx, svc, "'appDataFolder' in parents and name = '"+driveSnapshotPrefix+nodeID+"'", "files(id,name,appProperties)")
	if err != nil {
		return nil, fmt.Errorf("find the backup in Google Drive: %w", err)
	}
	if len(folders) != 1 {
		return nil, fmt.Errorf("Google Drive holds %d backups of node %s, expected one", len(folders), nodeID)
	}
	active := folders[0].AppProperties[driveActiveFolderProp]
	if active == "" {
		return nil, fmt.Errorf("the backup of node %s in Google Drive has no files", nodeID)
	}
	list, err := driveList(ctx, svc, fmt.Sprintf("'%s' in parents", active), "files(id,name,size)")
	if err != nil {
		return nil, fmt.Errorf("list the backup's files in Google Drive: %w", err)
	}
	wanted := map[string]bool{"backup.zip": true}
	for name := range nodeFileTargets("") {
		wanted[name] = true
	}
	b := &driveBackup{files: map[string][]byte{}, svc: svc, node: folders[0].Id}
	for _, f := range list {
		if !wanted[f.Name] {
			continue
		}
		if _, dup := b.files[f.Name]; dup {
			return nil, fmt.Errorf("the backup in Google Drive holds %s twice", f.Name)
		}
		res, err := svc.Files.Get(f.Id).Context(ctx).Download()
		if err != nil {
			return nil, fmt.Errorf("download %s from Google Drive: %w", f.Name, err)
		}
		content, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("download %s from Google Drive: %w", f.Name, err)
		}
		if f.Size > 0 && int64(len(content)) != f.Size {
			return nil, fmt.Errorf("download %s from Google Drive: got %d of %d bytes", f.Name, len(content), f.Size)
		}
		b.files[f.Name] = content
	}
	return b, nil
}

// markRestored records in Drive that this backup was restored elsewhere, as
// the library does on a restore (backup/drive.go:376-377): a phone that
// still runs this node stops itself on its next start instead of running
// the same channels in two places.
func (b *driveBackup) markRestored(ctx context.Context, instanceID string) error {
	update := &drive.File{AppProperties: map[string]string{driveBackupIDProp: instanceID}}
	if _, err := b.svc.Files.Update(b.node, update).Context(ctx).Do(); err != nil {
		return fmt.Errorf("mark the backup in Google Drive as restored: %w", err)
	}
	return nil
}

func driveList(ctx context.Context, svc *drive.Service, query string, fields string) ([]*drive.File, error) {
	var out []*drive.File
	pageToken := ""
	for {
		call := svc.Files.List().Spaces("appDataFolder").Context(ctx).
			Fields("nextPageToken", googleapi.Field(fields)).Q(query).PageSize(1000)
		if pageToken != "" {
			call = call.PageToken(pageToken)
		}
		r, err := call.Do()
		if err != nil {
			return nil, err
		}
		out = append(out, r.Files...)
		if pageToken = r.NextPageToken; pageToken == "" {
			return out, nil
		}
	}
}
