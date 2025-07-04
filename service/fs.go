/*
 @Version : 1.0
 @Author  : steven.wong
 @Email   : 'wangxk1991@gamil.com'
 @Time    : 2025/07/01 19:26:58
 Desc     : file system service
*/

package service

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	nurl "net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/piaobeizu/titan/config"
	"github.com/piaobeizu/titan/utils"
	"github.com/piaobeizu/titan/utils/cipher"
	"github.com/sirupsen/logrus"
)

var (
	ErrFileAlreadyExists    = errors.New("file already exists")
	ErrFileUploading        = errors.New("file is uploading")
	ErrUploadURLExpired     = errors.New("upload url expired")
	ErrMetaFileNotFound     = errors.New("meta file not found")
	ErrFileUploadIncomplete = errors.New("file upload incomplete")
)

func DefaultFileSystemConfig() *config.FileSystem {
	return &config.FileSystem{
		FileUploader: &config.FileUploader{
			FileMaxSize: 1024 * 1024 * 1024 * 100, // 100GB
			ChunkSize:   1024 * 1024 * 100,        // 100MB
			BufferSize:  1024 * 1024,              // 1MB
			FileTypes:   []string{"application/octet-stream", "image/jpeg", "image/png", "image/gif", "image/webp"},
			FormName:    "file",
			ExpireTime:  3600, // 1 hour
			PathMode:    0755, // 0755
		},
	}
}

type FileMeta struct {
	MD5     string `json:"md5"`
	Status  string `json:"status"`
	Expired int64  `json:"expired"`
	Type    string `json:"type"`
}

type FileSystem struct {
	ctx    context.Context
	config *config.FileSystem
	logger *logrus.Entry
}

func NewFileSystem(ctx context.Context, config *config.FileSystem) *FileSystem {
	return &FileSystem{
		ctx:    ctx,
		config: config,
		logger: logrus.WithField("module", "filesystem"),
	}
}

func (u *FileSystem) GenerateUploadURL(url, path, filename, secret string, pathParams []string) (string, error) {
	var (
		meta *FileMeta
		err  error
	)
	pathParams = append(pathParams, []string{
		fmt.Sprintf("file=%s", filename),
		fmt.Sprintf("path=%s", path),
	}...)
	secret, err = cipher.EncryptCompact([]byte(strings.Join(pathParams, "&")), secret)
	if err != nil {
		return "", err
	}
	pathParams = append(pathParams, fmt.Sprintf("secret=%s", secret))

	if meta, err = u.getFileMeta(path, filename); err == nil {
		meta.Expired = time.Now().Unix() + u.config.FileUploader.ExpireTime
	} else {
		meta = &FileMeta{
			MD5:     "",
			Status:  "incomplete",
			Expired: time.Now().Unix() + u.config.FileUploader.ExpireTime,
		}
	}
	if err := u.setFileMeta(path, filename, meta); err != nil {
		return "", err
	}
	u.logger.Infof("generate upload url: %s?%s", url, strings.Join(pathParams, "&"))
	return fmt.Sprintf("%s?%s", url, strings.Join(pathParams, "&")), nil
}

func (u *FileSystem) CheckUrl(urlObj *nurl.URL, secret string) error {
	if urlObj == nil {
		return fmt.Errorf("url is nil")
	}
	usecret := urlObj.Query().Get("secret")

	dsecret, err := cipher.DecryptCompact(usecret, secret)
	if err != nil {
		return fmt.Errorf("decrypt secret failed: %v", err)
	}

	ur, err := nurl.Parse("?" + string(dsecret))
	if err != nil {
		return fmt.Errorf("parse url failed: %v", err)
	}

	if ok, err := utils.EqualURL(ur.String(), urlObj.String(),
		[]string{"path", "kind", "name", "is_org", "mode", "force", "filename"}); !ok {
		return fmt.Errorf("invalid url: %s, please check your url", err.Error())
	}

	return nil
}

func (u *FileSystem) UploadFile(c *gin.Context, path, filename string, mode os.FileMode) (int64, error) {
	var (
		meta *FileMeta
		err  error
	)

	// TODO: 这个地方需要优化，文件锁在 getFileMeta 中获取，但是 setFileMeta 中没有释放，需要优化
	if meta, err = u.getFileMeta(path, filename); err != nil {
		u.logger.Errorf("get file meta failed: %s", err)
		return 0, ErrMetaFileNotFound
	}
	if meta.Status == "uploading" {
		return 0, ErrFileUploading
	}
	if meta.Status == "completed" {
		return 0, ErrFileAlreadyExists
	}
	if time.Now().Unix() > meta.Expired {
		return 0, ErrUploadURLExpired
	}

	meta.Status = "uploading"
	if err := u.setFileMeta(path, filename, meta); err != nil {
		return 0, err
	}

	if mode == 0 {
		mode = u.config.FileUploader.PathMode // Default file permissions
	}
	// upload file
	return u.uploadOSFile(c, path, filename, mode, meta)
}

func (u *FileSystem) ListDir(releasePath, path string, hidden bool) ([]map[string]any, error) {
	var items []map[string]any

	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	items = append(items, map[string]any{
		"path":     "/",
		"size":     info.Size(),
		"mode":     info.Mode(),
		"mod_time": info.ModTime().Format(time.DateTime),
		"is_dir":   true,
		"md5":      "",
	})
	for _, entry := range entries {
		// ignore hidden file
		if !hidden && strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		if hidden && strings.HasSuffix(entry.Name(), utils.GetEnv("GMI_FILE_META_SUFFIX", ".meta")) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		md5 := ""
		if !info.IsDir() {
			md5, err = u.MD5(path, entry.Name())
			if err != nil {
				return nil, err
			}
		}
		items = append(items, map[string]any{
			"path":     strings.TrimPrefix(entry.Name(), releasePath),
			"size":     info.Size(),
			"mode":     info.Mode(),
			"mod_time": info.ModTime().Format(time.DateTime),
			"is_dir":   entry.IsDir(),
			"md5":      md5,
		})
	}

	return items, nil
}

func (u *FileSystem) MD5(path, filename string) (string, error) {
	meta, err := u.getFileMeta(path, filename)
	if err != nil {
		return "", err
	}
	return meta.MD5, nil
}

func (u *FileSystem) uploadOSFile(c *gin.Context, path, filename string, mode os.FileMode, meta *FileMeta) (int64, error) {
	// check content type
	contentType := c.GetHeader("Content-Type")
	support := false
	for _, fileType := range u.config.FileUploader.FileTypes {
		if strings.HasPrefix(contentType, fileType) {
			support = true
			break
		}
	}
	if !support {
		return 0, fmt.Errorf("unsupported Content-Type: %s, we only support %s", contentType, strings.Join(u.config.FileUploader.FileTypes, ", "))
	}
	meta.Type = contentType

	// check content range
	contentRange := c.GetHeader("Content-Range")
	contentRangeMap, err := utils.ExtractByRegex(`^bytes (?P<start>\d+)-(?P<end>\d+)/(?P<total>\d+)$`, contentRange)
	if err != nil {
		return 0, fmt.Errorf("extract content range failed: %v", err)
	}
	totalSize, err := strconv.ParseInt(contentRangeMap["total"], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse total size failed: %v", err)
	}
	if totalSize > u.config.FileUploader.FileMaxSize {
		return 0, fmt.Errorf("file size exceeds the maximum limit: %d > %d", totalSize, u.config.FileUploader.FileMaxSize)
	}

	tempFilePath := utils.GetTempFilePath(path, filename)
	tmpFile, err := os.OpenFile(tempFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, mode)
	if err != nil {
		return 0, fmt.Errorf("open file failed: %v", err)
	}
	defer tmpFile.Close()

	defer func() {
		if err := u.setFileMeta(path, filename, meta); err != nil {
			u.logger.Errorf("set file meta failed: %s", err)
		}
	}()
	var (
		uploadSize int64
		buffer     = make([]byte, u.config.FileUploader.BufferSize)
		n          int
		uploadMB   int64
		hasher     = md5.New()
		md5Writer  = io.MultiWriter(tmpFile, hasher)
	)
mainloop:
	for {
		select {
		case <-u.ctx.Done():
			u.logger.Infof("❌ server context canceled, upload interrupted")
			err = fmt.Errorf("server context canceled")
			break mainloop
		case <-c.Request.Context().Done():
			u.logger.Infof("❌ client context canceled, connection interrupted")
			err = fmt.Errorf("client connection interrupted")
			break mainloop
		default:
			n, err = c.Request.Body.Read(buffer)
			if n > 0 {
				written, writeErr := md5Writer.Write(buffer[:n])
				if writeErr != nil {
					err = fmt.Errorf("write file failed: %v", writeErr)
					break mainloop
				}
				uploadSize += int64(written)

				if uploadSize/(1024*1024) != uploadMB { // 每1MB记录一次
					uploadMB = uploadSize / (1024 * 1024)
					u.logger.Infof("📊 file %s upload progress: %d MB", filename, uploadMB)
				}
			}
			if err == io.EOF {
				break mainloop
			}
			if err != nil {
				err = fmt.Errorf("read file failed: %v", err)
				break mainloop
			}
		}
	}
	if err != nil && err != io.EOF {
		return 0, err
	}
	md5Hash := fmt.Sprintf("%x", hasher.Sum(nil))
	meta.MD5 = md5Hash
	info, err := os.Stat(filepath.Join(path, filename))
	if err != nil {
		return info.Size(), err
	}
	if info.Size() != totalSize {
		meta.Status = "incomplete"
		return totalSize, ErrFileUploadIncomplete
	}
	os.Rename(tempFilePath, filepath.Join(path, filename))
	meta.Status = "completed"
	return totalSize, nil
}

func (u *FileSystem) getFileMeta(path, fileName string) (*FileMeta, error) {
	metaFile := u.getMetaPath(path, fileName)
	var meta FileMeta
	if err := utils.ReadFileToStruct(metaFile, &meta, "json", true); err != nil {
		return nil, err
	}
	return &meta, nil
}

func (u *FileSystem) setFileMeta(path, fileName string, meta *FileMeta) error {
	metaFile := u.getMetaPath(path, fileName)
	if meta == nil {
		meta = &FileMeta{
			MD5:     "",
			Status:  "unknown",
			Expired: 0,
		}
	}
	metaStr, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	if err := utils.WriteFile(metaFile, metaStr, 0644, true); err != nil {
		return err
	}
	return nil
}

func (u *FileSystem) getMetaPath(path, fileName string) string {
	fileName = strings.TrimPrefix(fileName, "/")
	fileName = strings.TrimPrefix(fileName, ".")
	return filepath.Join(path, fmt.Sprintf(".%s%s", fileName, utils.GetEnv("GMI_FILE_META_SUFFIX", ".meta")))
}
