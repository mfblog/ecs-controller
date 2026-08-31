package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
	"github.com/Kori1c/ecs-controller/internal/store"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	backupMagic            = "ECSBKP01"
	backupPasswordMinBytes = 8
	backupArchiveMaxBytes  = 64 << 20
	backupUploadMaxBytes   = 68 << 20
	backupKDFMemory        = 64 * 1024
	backupKDFIterations    = 1
	backupKDFThreads       = 2
	backupSaltSize         = 16
	backupNonceSize        = 24
	backupKeySize          = 32
)

type backupManifest struct {
	Format     string   `json:"format"`
	Version    int      `json:"version"`
	CreatedAt  string   `json:"created_at"`
	AppVersion string   `json:"app_version"`
	Tables     []string `json:"tables"`
	Files      []string `json:"files"`
}

type restoredBackup struct {
	SnapshotPath  string
	EncryptionKey [32]byte
	Logos         map[string][]byte
}

func validateBackupPassword(password string) error {
	if len(password) < backupPasswordMinBytes {
		return fmt.Errorf("备份密码至少需要 8 个字符")
	}
	return nil
}

func (s *Server) createBackup(w http.ResponseWriter, r *http.Request, data map[string]any) {
	password := stringValue(data["password"])
	if err := validateBackupPassword(password); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}

	s.backupMu.Lock()
	defer s.backupMu.Unlock()

	tmp, err := os.CreateTemp(s.DataDir, ".ecs-backup-snapshot-*.sqlite")
	if err != nil {
		s.error(w, http.StatusInternalServerError, "无法创建备份临时文件")
		return
	}
	snapshotPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(snapshotPath)

	if err := s.Store.Snapshot(snapshotPath); err != nil {
		s.error(w, http.StatusInternalServerError, "数据库快照创建失败: "+err.Error())
		return
	}
	archive, err := buildBackupArchive(snapshotPath, s.DataDir, s.Store.EncryptionKey())
	if err != nil {
		s.error(w, http.StatusInternalServerError, "备份打包失败: "+err.Error())
		return
	}
	encrypted, err := encryptBackup(archive, password)
	if err != nil {
		s.error(w, http.StatusInternalServerError, "备份加密失败: "+err.Error())
		return
	}

	filename := "ecs-controller-backup-" + time.Now().Format("20060102-150405") + ".ecsbkp"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	w.Header().Set("Content-Length", fmt.Sprint(len(encrypted)))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encrypted)
}

func (s *Server) restoreBackup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, backupUploadMaxBytes)
	if err := r.ParseMultipartForm(backupUploadMaxBytes); err != nil {
		s.error(w, http.StatusBadRequest, "备份文件过大或格式无效")
		return
	}
	password := r.FormValue("password")
	if err := validateBackupPassword(password); err != nil {
		s.error(w, http.StatusBadRequest, err.Error())
		return
	}
	file, _, err := r.FormFile("backup")
	if err != nil {
		s.error(w, http.StatusBadRequest, "请选择有效的备份文件")
		return
	}
	defer file.Close()
	encrypted, err := io.ReadAll(io.LimitReader(file, backupUploadMaxBytes+1))
	if err != nil || len(encrypted) > backupUploadMaxBytes {
		s.error(w, http.StatusBadRequest, "备份文件过大")
		return
	}

	s.backupMu.Lock()
	defer s.backupMu.Unlock()
	tmpDir, err := os.MkdirTemp(s.DataDir, ".ecs-restore-*")
	if err != nil {
		s.error(w, http.StatusInternalServerError, "无法创建恢复临时目录")
		return
	}
	defer os.RemoveAll(tmpDir)
	restored, err := extractBackup(encrypted, password, tmpDir)
	if err != nil {
		s.error(w, http.StatusBadRequest, "备份校验失败: "+err.Error())
		return
	}
	if err := s.Store.RestoreSnapshot(restored.SnapshotPath, restored.EncryptionKey); err != nil {
		s.error(w, http.StatusInternalServerError, "恢复数据库失败: "+err.Error())
		return
	}
	logoErr := replaceBrandLogos(s.DataDir, restored.Logos)
	// The database is already restored at this point. Keep the result usable if
	// an optional custom Logo cannot be written, while recording the detail.
	message := "备份已恢复，请使用备份中的管理员凭据重新登录"
	if logoErr != nil {
		s.Store.AddLog("warning", "备份已恢复，但 Logo 文件恢复失败: "+logoErr.Error())
		message += "；Logo 恢复失败"
	}
	http.SetCookie(w, &http.Cookie{Name: "ecs_session", MaxAge: -1, Path: "/"})
	s.json(w, http.StatusOK, map[string]any{"success": true, "message": message})
}

func buildBackupArchive(snapshotPath, dataDir string, encryptionKey [32]byte) ([]byte, error) {
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gz)
	manifest := backupManifest{
		Format:     "ecs-controller-backup",
		Version:    1,
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		AppVersion: app.Version,
		Tables:     store.BackupTables(),
		Files:      []string{"manifest.json", "data.sqlite", ".secret_encryption.key"},
	}
	logos := []string{"brand-logo.png", "brand-logo.jpg", "brand-logo.webp"}
	for _, name := range logos {
		if _, err := os.Stat(filepath.Join(dataDir, name)); err == nil {
			manifest.Files = append(manifest.Files, name)
		}
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := writeTarBytes(tarWriter, "manifest.json", manifestRaw, 0600); err != nil {
		return nil, err
	}
	if err := writeTarFile(tarWriter, "data.sqlite", snapshotPath, backupArchiveMaxBytes); err != nil {
		return nil, err
	}
	if err := writeTarBytes(tarWriter, ".secret_encryption.key", encryptionKey[:], 0600); err != nil {
		return nil, err
	}
	for _, name := range logos {
		path := filepath.Join(dataDir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := writeTarFile(tarWriter, name, path, 2<<20); err != nil {
			return nil, err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > backupArchiveMaxBytes {
		return nil, fmt.Errorf("备份内容超过 %d MB", backupArchiveMaxBytes/(1<<20))
	}
	return compressed.Bytes(), nil
}

func writeTarBytes(writer *tar.Writer, name string, data []byte, mode int64) error {
	header := &tar.Header{Name: name, Mode: mode, Size: int64(len(data)), ModTime: time.Now().UTC()}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}
	_, err := writer.Write(data)
	return err
}

func writeTarFile(writer *tar.Writer, name, path string, maxBytes int64) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return fmt.Errorf("文件 %s 无效或过大", name)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), ModTime: time.Now().UTC()}); err != nil {
		return err
	}
	_, err = io.Copy(writer, file)
	return err
}

func encryptBackup(archive []byte, password string) ([]byte, error) {
	if len(archive) > backupArchiveMaxBytes {
		return nil, fmt.Errorf("备份内容过大")
	}
	var salt [backupSaltSize]byte
	var nonce [backupNonceSize]byte
	if _, err := rand.Read(salt[:]); err != nil {
		return nil, err
	}
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	keyBytes := argon2.IDKey([]byte(password), salt[:], backupKDFIterations, backupKDFMemory, backupKDFThreads, backupKeySize)
	var key [32]byte
	copy(key[:], keyBytes)
	ciphertext := secretbox.Seal(nil, archive, &nonce, &key)
	result := make([]byte, 0, len(backupMagic)+len(salt)+len(nonce)+len(ciphertext))
	result = append(result, backupMagic...)
	result = append(result, salt[:]...)
	result = append(result, nonce[:]...)
	result = append(result, ciphertext...)
	return result, nil
}

func decryptBackup(encrypted []byte, password string) ([]byte, error) {
	headerSize := len(backupMagic) + backupSaltSize + backupNonceSize
	if len(encrypted) <= headerSize+secretbox.Overhead || string(encrypted[:minInt(len(encrypted), len(backupMagic))]) != backupMagic {
		return nil, fmt.Errorf("不是有效的 ECS 控制台备份文件")
	}
	if len(encrypted) > backupUploadMaxBytes {
		return nil, fmt.Errorf("备份文件过大")
	}
	var salt [backupSaltSize]byte
	var nonce [backupNonceSize]byte
	copy(salt[:], encrypted[len(backupMagic):len(backupMagic)+backupSaltSize])
	copy(nonce[:], encrypted[len(backupMagic)+backupSaltSize:headerSize])
	keyBytes := argon2.IDKey([]byte(password), salt[:], backupKDFIterations, backupKDFMemory, backupKDFThreads, backupKeySize)
	var key [32]byte
	copy(key[:], keyBytes)
	plain, ok := secretbox.Open(nil, encrypted[headerSize:], &nonce, &key)
	if !ok {
		return nil, fmt.Errorf("备份密码错误或文件已损坏")
	}
	if len(plain) > backupArchiveMaxBytes {
		return nil, fmt.Errorf("解密后的备份过大")
	}
	return plain, nil
}

func extractBackup(encrypted []byte, password, tempDir string) (*restoredBackup, error) {
	plain, err := decryptBackup(encrypted, password)
	if err != nil {
		return nil, err
	}
	gz, err := gzip.NewReader(bytes.NewReader(plain))
	if err != nil {
		return nil, fmt.Errorf("压缩数据无效")
	}
	defer gz.Close()
	tarReader := tar.NewReader(gz)
	var manifest backupManifest
	manifestFound := false
	keyFound := false
	seen := map[string]bool{}
	result := &restoredBackup{Logos: map[string][]byte{}}
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("备份归档无效")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			return nil, fmt.Errorf("备份包含不支持的文件类型")
		}
		if header.Size < 0 || header.Size > backupArchiveMaxBytes || seen[header.Name] {
			return nil, fmt.Errorf("备份文件条目无效")
		}
		seen[header.Name] = true
		switch header.Name {
		case "manifest.json":
			data, readErr := readTarEntry(tarReader, header.Size, 1<<20)
			if readErr != nil || json.Unmarshal(data, &manifest) != nil {
				return nil, fmt.Errorf("备份清单无效")
			}
			manifestFound = true
		case "data.sqlite":
			path := filepath.Join(tempDir, "data.sqlite")
			if err := writeTarEntry(path, tarReader, header.Size, backupArchiveMaxBytes); err != nil {
				return nil, fmt.Errorf("数据库快照无效: %w", err)
			}
			result.SnapshotPath = path
		case ".secret_encryption.key":
			data, readErr := readTarEntry(tarReader, header.Size, backupKeySize)
			if readErr != nil || len(data) != backupKeySize {
				return nil, fmt.Errorf("备份密钥无效")
			}
			copy(result.EncryptionKey[:], data)
			keyFound = true
		case "brand-logo.png", "brand-logo.jpg", "brand-logo.webp":
			data, readErr := readTarEntry(tarReader, header.Size, 2<<20)
			if readErr != nil {
				return nil, fmt.Errorf("Logo 文件无效")
			}
			result.Logos[header.Name] = data
		default:
			return nil, fmt.Errorf("备份包含未知文件")
		}
	}
	if !manifestFound || manifest.Format != "ecs-controller-backup" || manifest.Version != 1 || !keyFound || result.SnapshotPath == "" {
		return nil, fmt.Errorf("备份版本或内容不完整")
	}
	return result, nil
}

func readTarEntry(reader *tar.Reader, size, max int64) ([]byte, error) {
	if size < 0 || size > max {
		return nil, fmt.Errorf("条目过大")
	}
	data, err := io.ReadAll(io.LimitReader(reader, size+1))
	if err != nil || int64(len(data)) != size {
		return nil, fmt.Errorf("条目读取失败")
	}
	return data, nil
}

func writeTarEntry(path string, reader *tar.Reader, size, max int64) error {
	if size < 0 || size > max {
		return fmt.Errorf("条目过大")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := io.CopyN(file, reader, size); err != nil {
		return err
	}
	return nil
}

func replaceBrandLogos(dataDir string, logos map[string][]byte) error {
	type stagedLogo struct{ name, path string }
	staged := make([]stagedLogo, 0, len(logos))
	for name, data := range logos {
		tmp, err := os.CreateTemp(dataDir, ".ecs-brand-logo-*")
		if err != nil {
			return err
		}
		path := tmp.Name()
		if _, err := tmp.Write(data); err != nil {
			tmp.Close()
			os.Remove(path)
			return err
		}
		if err := tmp.Chmod(0600); err != nil {
			tmp.Close()
			os.Remove(path)
			return err
		}
		if err := tmp.Close(); err != nil {
			os.Remove(path)
			return err
		}
		staged = append(staged, stagedLogo{name: name, path: path})
	}
	for _, ext := range []string{"png", "jpg", "webp"} {
		if err := os.Remove(filepath.Join(dataDir, "brand-logo."+ext)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, item := range staged {
		if err := os.Rename(item.path, filepath.Join(dataDir, item.name)); err != nil {
			return err
		}
	}
	return nil
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
