package backup

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Result struct {
	FilePath  string
	SizeBytes int64
	SHA256    string
}

type S3Config struct {
	Endpoint     string
	Region       string
	Bucket       string
	AccessKey    string
	SecretKey    string
	SessionToken string
	UseSSL       bool
	Prefix       string
}

func quoteSQLitePath(path string) string {
	return strings.ReplaceAll(path, "'", "''")
}

func CreateSQLiteBackup(ctx context.Context, db *sql.DB, dbPath, backupDir string) (Result, error) {
	if strings.TrimSpace(backupDir) == "" {
		backupDir = "backups"
	}
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		return Result{}, err
	}
	ts := time.Now().UTC().Format("20060102T150405Z")
	rawPath := filepath.Join(backupDir, "neutrino-"+ts+".sqlite")
	gzPath := rawPath + ".gz"

	if _, err := db.ExecContext(ctx, fmt.Sprintf("VACUUM INTO '%s';", quoteSQLitePath(rawPath))); err != nil {
		return Result{}, err
	}
	defer os.Remove(rawPath)

	in, err := os.Open(rawPath)
	if err != nil {
		return Result{}, err
	}
	defer in.Close()

	out, err := os.Create(gzPath)
	if err != nil {
		return Result{}, err
	}
	h := sha256.New()
	mw := io.MultiWriter(out, h)
	zw := gzip.NewWriter(mw)
	if _, err := io.Copy(zw, in); err != nil {
		_ = zw.Close()
		_ = out.Close()
		return Result{}, err
	}
	if err := zw.Close(); err != nil {
		_ = out.Close()
		return Result{}, err
	}
	if err := out.Close(); err != nil {
		return Result{}, err
	}
	info, err := os.Stat(gzPath)
	if err != nil {
		return Result{}, err
	}
	return Result{FilePath: gzPath, SizeBytes: info.Size(), SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

func RestoreSQLiteBackup(gzipPath, dbPath string) error {
	in, err := os.Open(gzipPath)
	if err != nil {
		return err
	}
	defer in.Close()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return err
	}
	defer zr.Close()

	tmpPath := dbPath + ".restore.tmp"
	out, err := os.Create(tmpPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, zr); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dbPath); err != nil {
		return err
	}
	return nil
}

func openS3Client(cfg S3Config) (*minio.Client, error) {
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	endpoint = strings.TrimPrefix(endpoint, "https://")
	endpoint = strings.TrimPrefix(endpoint, "http://")
	useSSL := cfg.UseSSL
	if strings.HasPrefix(strings.TrimSpace(cfg.Endpoint), "http://") {
		useSSL = false
	}
	creds := credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken)
	if strings.TrimSpace(cfg.AccessKey) == "" && strings.TrimSpace(cfg.SecretKey) == "" {
		creds = credentials.NewIAM("")
	}
	return minio.New(endpoint, &minio.Options{
		Creds:  creds,
		Secure: useSSL,
		Region: cfg.Region,
	})
}

func UploadFileToS3(ctx context.Context, cfg S3Config, localPath string) (string, error) {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return "", fmt.Errorf("empty s3 bucket")
	}
	client, err := openS3Client(cfg)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(localPath) == "" {
		return "", fmt.Errorf("empty local path")
	}
	key := filepath.Base(localPath)
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	if prefix != "" {
		key = prefix + "/" + key
	}
	_, err = client.FPutObject(ctx, cfg.Bucket, key, localPath, minio.PutObjectOptions{
		ContentType: "application/gzip",
	})
	if err != nil {
		return "", err
	}
	return key, nil
}

func DownloadFileFromS3(ctx context.Context, cfg S3Config, objectKey, dstPath string) error {
	if strings.TrimSpace(cfg.Bucket) == "" {
		return fmt.Errorf("empty s3 bucket")
	}
	if strings.TrimSpace(objectKey) == "" {
		return fmt.Errorf("empty object key")
	}
	if strings.TrimSpace(dstPath) == "" {
		return fmt.Errorf("empty dst path")
	}
	client, err := openS3Client(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return err
	}
	return client.FGetObject(ctx, cfg.Bucket, objectKey, dstPath, minio.GetObjectOptions{})
}
