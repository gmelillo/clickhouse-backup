package backup

import (
	"context"
	"errors"
	"fmt"
	"github.com/Altinity/clickhouse-backup/v2/pkg/metadata"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/Altinity/clickhouse-backup/v2/pkg/clickhouse"
	"github.com/Altinity/clickhouse-backup/v2/pkg/config"
	"github.com/Altinity/clickhouse-backup/v2/pkg/resumable"
	"github.com/Altinity/clickhouse-backup/v2/pkg/storage"

	apexLog "github.com/apex/log"
)

const DirectoryFormat = "directory"

var errShardOperationUnsupported = errors.New("sharded operations are not supported")

// versioner is an interface for determining the version of Clickhouse
type versioner interface {
	CanShardOperation(ctx context.Context) error
}

type BackuperOpt func(*Backuper)

type Backuper struct {
	cfg                    *config.Config
	ch                     *clickhouse.ClickHouse
	vers                   versioner
	bs                     backupSharder
	dst                    *storage.BackupDestination
	log                    *apexLog.Entry
	DiskToPathMap          map[string]string
	DefaultDataPath        string
	EmbeddedBackupDataPath string
	isEmbedded             bool
	resume                 bool
	resumableState         *resumable.State
}

func NewBackuper(cfg *config.Config, opts ...BackuperOpt) *Backuper {
	ch := &clickhouse.ClickHouse{
		Config: &cfg.ClickHouse,
		Log:    apexLog.WithField("logger", "clickhouse"),
	}
	b := &Backuper{
		cfg:  cfg,
		ch:   ch,
		vers: ch,
		bs:   nil,
		log:  apexLog.WithField("logger", "backuper"),
	}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

func WithVersioner(v versioner) BackuperOpt {
	return func(b *Backuper) {
		b.vers = v
	}
}

func WithBackupSharder(s backupSharder) BackuperOpt {
	return func(b *Backuper) {
		b.bs = s
	}
}

func (b *Backuper) initDisksPathdsAndBackupDestination(ctx context.Context, disks []clickhouse.Disk, backupName string) error {
	var err error
	if disks == nil {
		disks, err = b.ch.GetDisks(ctx, true)
		if err != nil {
			return err
		}
	}
	b.DefaultDataPath, err = b.ch.GetDefaultPath(disks)
	if err != nil {
		return ErrUnknownClickhouseDataPath
	}
	diskMap := map[string]string{}
	for _, disk := range disks {
		diskMap[disk.Name] = disk.Path
		if b.cfg.ClickHouse.UseEmbeddedBackupRestore && (disk.IsBackup || disk.Name == b.cfg.ClickHouse.EmbeddedBackupDisk) {
			b.EmbeddedBackupDataPath = disk.Path
		}
	}
	if b.cfg.ClickHouse.UseEmbeddedBackupRestore && b.EmbeddedBackupDataPath == "" {
		b.EmbeddedBackupDataPath = b.DefaultDataPath
	}
	b.DiskToPathMap = diskMap
	if b.cfg.General.RemoteStorage != "none" && b.cfg.General.RemoteStorage != "custom" {
		b.dst, err = storage.NewBackupDestination(ctx, b.cfg, b.ch, true, backupName)
		if err != nil {
			return err
		}
		if err := b.dst.Connect(ctx); err != nil {
			return fmt.Errorf("can't connect to %s: %v", b.dst.Kind(), err)
		}
	}
	return nil
}

func (b *Backuper) getLocalBackupDataPathForTable(backupName string, disk string, dbAndTablePath string) string {
	backupPath := path.Join(b.DiskToPathMap[disk], "backup", backupName, "shadow", dbAndTablePath, disk)
	if b.isEmbedded {
		backupPath = path.Join(b.DiskToPathMap[disk], backupName, "data", dbAndTablePath)
	}
	return backupPath
}

// populateBackupShardField populates the BackupShard field for a slice of Table structs
func (b *Backuper) populateBackupShardField(ctx context.Context, tables []clickhouse.Table) error {
	// By default, have all fields populated to full backup unless the table is to be skipped
	for i := range tables {
		tables[i].BackupType = clickhouse.ShardBackupFull
		if tables[i].Skip {
			tables[i].BackupType = clickhouse.ShardBackupNone
		}
	}
	if !doesShard(b.cfg.General.ShardedOperationMode) {
		return nil
	}
	if err := b.vers.CanShardOperation(ctx); err != nil {
		return err
	}

	if b.bs == nil {
		// Parse shard config here to avoid error return in NewBackuper
		shardFunc, err := shardFuncByName(b.cfg.General.ShardedOperationMode)
		if err != nil {
			return fmt.Errorf("could not determine shards for tables: %w", err)
		}
		b.bs = newReplicaDeterminer(b.ch, shardFunc)
	}
	assignment, err := b.bs.determineShards(ctx)
	if err != nil {
		return err
	}
	for i, t := range tables {
		if t.Skip {
			continue
		}
		fullBackup, err := assignment.inShard(t.Database, t.Name)
		if err != nil {
			return err
		}
		if !fullBackup {
			tables[i].BackupType = clickhouse.ShardBackupSchema
		}
	}
	return nil
}

func (b *Backuper) isDiskTypeObject(diskType string) bool {
	return diskType == "s3" || diskType == "azure_blob_storage" || diskType == "azure"
}

func (b *Backuper) isDiskTypeEncryptedObject(disk clickhouse.Disk, disks []clickhouse.Disk) bool {
	if disk.Type != "encrypted" {
		return false
	}
	underlyingIdx := -1
	underlyingPath := ""
	for i, d := range disks {
		if d.Name != disk.Name && strings.HasPrefix(disk.Path, d.Path) && b.isDiskTypeObject(d.Type) {
			if d.Path > underlyingPath {
				underlyingIdx = i
				underlyingPath = d.Path
			}
		}
	}
	return underlyingIdx >= 0
}

func (b *Backuper) getEmbeddedBackupLocation(ctx context.Context, backupName string) (string, error) {
	if b.cfg.ClickHouse.EmbeddedBackupDisk != "" {
		return fmt.Sprintf("Disk('%s','%s')", b.cfg.ClickHouse.EmbeddedBackupDisk, backupName), nil
	}

	if err := b.applyMacrosToObjectDiskPath(ctx); err != nil {
		return "", err
	}
	if b.cfg.General.RemoteStorage == "s3" {
		s3Endpoint, err := b.ch.ApplyMacros(ctx, b.buildEmbeddedLocationS3())
		if err != nil {
			return "", err
		}
		if b.cfg.S3.AccessKey != "" {
			return fmt.Sprintf("S3('%s/%s','%s','%s')", s3Endpoint, backupName, b.cfg.S3.AccessKey, b.cfg.S3.SecretKey), nil
		}
		if os.Getenv("AWS_ACCESS_KEY_ID") != "" {
			return fmt.Sprintf("S3('%s/%s','%s','%s')", s3Endpoint, backupName, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")), nil
		}
		return "", fmt.Errorf("provide s3->access_key and s3->secret_key in config to allow embedded backup without `clickhouse->embedded_backup_disk`")
	}
	if b.cfg.General.RemoteStorage == "gcs" {
		gcsEndpoint, err := b.ch.ApplyMacros(ctx, b.buildEmbeddedLocationGCS())
		if err != nil {
			return "", err
		}
		if b.cfg.GCS.EmbeddedAccessKey != "" {
			return fmt.Sprintf("S3('%s/%s','%s','%s')", gcsEndpoint, backupName, b.cfg.GCS.EmbeddedAccessKey, b.cfg.GCS.EmbeddedSecretKey), nil
		}
		if os.Getenv("AWS_ACCESS_KEY_ID") != "" {
			return fmt.Sprintf("S3('%s/%s','%s','%s')", gcsEndpoint, backupName, os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")), nil
		}
		return "", fmt.Errorf("provide gcs->embedded_access_key and gcs->embedded_secret_key in config to allow embedded backup without `clickhouse->embedded_backup_disk`")

	}
	if b.cfg.General.RemoteStorage == "azblob" {
		azblobEndpoint, err := b.ch.ApplyMacros(ctx, b.buildEmbeddedLocationAZBLOB())
		if err != nil {
			return "", err
		}
		if b.cfg.AzureBlob.Container != "" {
			return fmt.Sprintf("AzureBlobStorage('%s','%s','%s/%s')", azblobEndpoint, b.cfg.AzureBlob.Container, b.cfg.AzureBlob.ObjectDiskPath, backupName), nil
		}
		return "", fmt.Errorf("provide azblob->container and azblob->account_name, azblob->account_key in config to allow embedded backup without `clickhouse->embedded_backup_disk`")
	}
	return "", fmt.Errorf("empty clickhouse->embedded_backup_disk and invalid general->remote_storage: %s", b.cfg.General.RemoteStorage)
}

func (b *Backuper) applyMacrosToObjectDiskPath(ctx context.Context) error {
	var err error
	if b.cfg.General.RemoteStorage == "s3" {
		b.cfg.S3.ObjectDiskPath, err = b.ch.ApplyMacros(ctx, b.cfg.S3.ObjectDiskPath)
	} else if b.cfg.General.RemoteStorage == "gcs" {
		b.cfg.GCS.ObjectDiskPath, err = b.ch.ApplyMacros(ctx, b.cfg.GCS.ObjectDiskPath)
	} else if b.cfg.General.RemoteStorage == "azblob" {
		b.cfg.AzureBlob.ObjectDiskPath, err = b.ch.ApplyMacros(ctx, b.cfg.AzureBlob.ObjectDiskPath)
	}
	return err
}

func (b *Backuper) buildEmbeddedLocationS3() string {
	url := url.URL{}
	url.Scheme = "https"
	if strings.HasPrefix(b.cfg.S3.Endpoint, "http") {
		newUrl, _ := url.Parse(b.cfg.S3.Endpoint)
		url = *newUrl
		url.Path = path.Join(b.cfg.S3.Bucket, b.cfg.S3.ObjectDiskPath)
	} else {
		url.Host = b.cfg.S3.Endpoint
		url.Path = path.Join(b.cfg.S3.Bucket, b.cfg.S3.ObjectDiskPath)
	}
	if b.cfg.S3.DisableSSL {
		url.Scheme = "http"
	}
	if url.Host == "" && b.cfg.S3.Region != "" && b.cfg.S3.ForcePathStyle {
		url.Host = "s3." + b.cfg.S3.Region + ".amazonaws.com"
		url.Path = path.Join(b.cfg.S3.Bucket, b.cfg.S3.ObjectDiskPath)
	}
	if url.Host == "" && b.cfg.S3.Bucket != "" && !b.cfg.S3.ForcePathStyle {
		url.Host = b.cfg.S3.Bucket + "." + "s3." + b.cfg.S3.Region + ".amazonaws.com"
		url.Path = b.cfg.S3.ObjectDiskPath
	}
	return url.String()
}

func (b *Backuper) buildEmbeddedLocationGCS() string {
	url := url.URL{}
	url.Scheme = "https"
	if b.cfg.GCS.ForceHttp {
		url.Scheme = "http"
	}
	if b.cfg.GCS.Endpoint != "" {
		if !strings.HasPrefix(b.cfg.GCS.Endpoint, "http") {
			url.Host = b.cfg.GCS.Endpoint
		} else {
			newUrl, _ := url.Parse(b.cfg.GCS.Endpoint)
			url = *newUrl
		}
	}
	if url.Host == "" {
		url.Host = "storage.googleapis.com"
	}
	url.Path = path.Join(b.cfg.GCS.Bucket, b.cfg.GCS.ObjectDiskPath)
	return url.String()
}

func (b *Backuper) buildEmbeddedLocationAZBLOB() string {
	url := url.URL{}
	url.Scheme = b.cfg.AzureBlob.EndpointSchema
	url.Host = b.cfg.AzureBlob.EndpointSuffix
	url.Path = b.cfg.AzureBlob.AccountName
	return fmt.Sprintf("DefaultEndpointsProtocol=%s;AccountName=%s;AccountKey=%s;BlobEndpoint=%s;", b.cfg.AzureBlob.EndpointSchema, b.cfg.AzureBlob.AccountName, b.cfg.AzureBlob.AccountKey, url.String())
}

func (b *Backuper) getObjectDiskPath() (string, error) {
	if b.cfg.General.RemoteStorage == "s3" {
		return b.cfg.S3.ObjectDiskPath, nil
	} else if b.cfg.General.RemoteStorage == "azblob" {
		return b.cfg.AzureBlob.ObjectDiskPath, nil
	} else if b.cfg.General.RemoteStorage == "gcs" {
		return b.cfg.GCS.ObjectDiskPath, nil
	} else {
		return "", fmt.Errorf("cleanBackupObjectDisks: requesst object disks path but have unsupported remote_storage: %s", b.cfg.General.RemoteStorage)
	}
}

func (b *Backuper) getTablesDiffFromLocal(ctx context.Context, diffFrom string, tablePattern string) (tablesForUploadFromDiff map[metadata.TableTitle]metadata.TableMetadata, err error) {
	tablesForUploadFromDiff = make(map[metadata.TableTitle]metadata.TableMetadata)
	diffFromBackup, err := b.ReadBackupMetadataLocal(ctx, diffFrom)
	if err != nil {
		return nil, err
	}
	if len(diffFromBackup.Tables) != 0 {
		metadataPath := path.Join(b.DefaultDataPath, "backup", diffFrom, "metadata")
		// empty partitions, because we don't want filter
		diffTablesList, _, err := b.getTableListByPatternLocal(ctx, metadataPath, tablePattern, false, []string{})
		if err != nil {
			return nil, err
		}
		for _, t := range diffTablesList {
			tablesForUploadFromDiff[metadata.TableTitle{
				Database: t.Database,
				Table:    t.Table,
			}] = t
		}
	}
	return tablesForUploadFromDiff, nil
}

func (b *Backuper) getTablesDiffFromRemote(ctx context.Context, diffFromRemote string, tablePattern string) (tablesForUploadFromDiff map[metadata.TableTitle]metadata.TableMetadata, err error) {
	tablesForUploadFromDiff = make(map[metadata.TableTitle]metadata.TableMetadata)
	backupList, err := b.dst.BackupList(ctx, true, diffFromRemote)
	if err != nil {
		return nil, err
	}
	var diffRemoteMetadata *metadata.BackupMetadata
	for _, backup := range backupList {
		if backup.BackupName == diffFromRemote {
			diffRemoteMetadata = &backup.BackupMetadata
			break
		}
	}
	if diffRemoteMetadata == nil {
		return nil, fmt.Errorf("%s not found on remote storage", diffFromRemote)
	}

	if len(diffRemoteMetadata.Tables) != 0 {
		diffTablesList, err := getTableListByPatternRemote(ctx, b, diffRemoteMetadata, tablePattern, false)
		if err != nil {
			return nil, err
		}
		for _, t := range diffTablesList {
			tablesForUploadFromDiff[metadata.TableTitle{
				Database: t.Database,
				Table:    t.Table,
			}] = t
		}
	}
	return tablesForUploadFromDiff, nil
}
