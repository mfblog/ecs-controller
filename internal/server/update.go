package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kori1c/ecs-controller/internal/app"
)

const (
	defaultUpdateRepo   = "Kori1c/ecs-controller"
	defaultUpdateBranch = "main"
	defaultImageRepo    = "docker.io/kori1c/ecs-controller"
)

var commitPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var versionTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

type updateCheckResult struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
	} `json:"commit"`
	HTMLURL string `json:"html_url"`
}

type updateVersionTag struct {
	Name   string `json:"name"`
	Commit struct {
		SHA string `json:"sha"`
	} `json:"commit"`
}

type updateRequest struct {
	RequestID   string `json:"request_id"`
	TargetSHA   string `json:"target_sha"`
	RequestedAt int64  `json:"requested_at"`
}

func (s *Server) updateRepository() string {
	return fallback(os.Getenv("ECS_UPDATE_REPO"), defaultUpdateRepo)
}

func (s *Server) updateBranch() string {
	return fallback(os.Getenv("ECS_UPDATE_BRANCH"), defaultUpdateBranch)
}

func (s *Server) updateRepositoryURL() string {
	return "https://github.com/" + s.updateRepository()
}

func (s *Server) updateAPIURL() string {
	if base := strings.TrimRight(strings.TrimSpace(s.githubAPIBase), "/"); base != "" {
		return base
	}
	return "https://api.github.com"
}

func (s *Server) updateImageRepository() string {
	return fallback(os.Getenv("ECS_IMAGE_REPOSITORY"), defaultImageRepo)
}

func imageTag(commit string) string {
	return "sha-" + strings.ToLower(strings.TrimSpace(commit))
}

func (s *Server) updateConfigured() bool {
	return strings.TrimSpace(s.UpdateDir) != ""
}

func (s *Server) checkForUpdate(w http.ResponseWriter, r *http.Request) {
	currentCommit := strings.TrimSpace(app.Commit)
	currentVersion := shortCommit(currentCommit)
	if currentVersion == "" || currentVersion == "dev" {
		currentVersion = app.Version
	}
	result := map[string]any{
		"success":           true,
		"configured":        s.updateConfigured(),
		"repository":        s.updateRepository(),
		"repository_url":    s.updateRepositoryURL(),
		"image_repository":  s.updateImageRepository(),
		"branch":            s.updateBranch(),
		"current_version":   currentVersion,
		"current_commit":    currentCommit,
		"current_url":       "",
		"current_image_tag": "",
		"build_date":        app.BuildDate,
		"update_available":  false,
		"checked_at":        time.Now().UTC().Format(time.RFC3339),
	}
	if commitPattern.MatchString(currentCommit) {
		result["current_url"] = s.updateRepositoryURL() + "/commit/" + currentCommit
		result["current_image_tag"] = imageTag(currentCommit)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	endpoint := fmt.Sprintf("%s/repos/%s/commits/%s", s.updateAPIURL(), s.updateRepository(), s.updateBranch())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		s.error(w, http.StatusBadGateway, "更新检查请求创建失败")
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ecs-controller-update-check")
	resp, err := (&http.Client{Timeout: 8 * time.Second}).Do(req)
	if err != nil {
		result["success"] = false
		result["message"] = "无法连接 GitHub，请检查容器网络"
		s.json(w, http.StatusOK, result)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		result["success"] = false
		result["message"] = fmt.Sprintf("GitHub 返回 HTTP %d", resp.StatusCode)
		s.json(w, http.StatusOK, result)
		return
	}
	var latest updateCheckResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&latest); err != nil || !commitPattern.MatchString(latest.SHA) {
		result["success"] = false
		result["message"] = "GitHub 返回的版本信息无效"
		s.json(w, http.StatusOK, result)
		return
	}
	versionTags := s.versionTags(ctx)
	if version := versionTags[strings.ToLower(currentCommit)]; version != "" {
		result["current_version"] = version
	}
	latestVersion := fallback(versionTags[strings.ToLower(latest.SHA)], shortCommit(latest.SHA))
	result["latest"] = map[string]any{
		"version": latestVersion,
		"commit":  latest.SHA,
		"message": strings.TrimSpace(strings.Split(latest.Commit.Message, "\n")[0]),
		"url":     latest.HTMLURL,
	}
	sourceUpdateAvailable := !strings.EqualFold(currentCommit, latest.SHA) && !strings.EqualFold(currentCommit, shortCommit(latest.SHA))
	result["source_update_available"] = sourceUpdateAvailable
	result["image_tag"] = imageTag(latest.SHA)
	imageAvailable, imageDigest, imageErr := s.prebuiltImageAvailable(ctx, latest.SHA)
	result["image_available"] = imageAvailable
	if imageDigest != "" {
		result["image_digest"] = imageDigest
	}
	if imageErr != nil {
		result["image_check_error"] = imageErr.Error()
	}
	result["update_available"] = sourceUpdateAvailable && imageAvailable
	if sourceUpdateAvailable && !imageAvailable {
		if imageErr != nil {
			result["message"] = "检测到源码更新，但无法确认对应的预构建 Docker 镜像，请稍后重试"
		} else {
			result["message"] = "检测到源码更新，Docker 镜像正在构建中····，请稍等片刻～"
		}
	}
	if !s.updateConfigured() {
		result["message"] = "当前部署未启用 Docker 在线更新，请使用 install.sh 更新"
	}
	s.json(w, http.StatusOK, result)
}

func (s *Server) versionTags(ctx context.Context) map[string]string {
	endpoint := fmt.Sprintf("%s/repos/%s/tags?per_page=100", s.updateAPIURL(), s.updateRepository())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "ecs-controller-update-check")
	response, err := (&http.Client{Timeout: 8 * time.Second}).Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil
	}
	var tags []updateVersionTag
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tags); err != nil {
		return nil
	}
	return versionTagsForCommits(tags)
}

func versionTagsForCommits(tags []updateVersionTag) map[string]string {
	versions := make(map[string]string, len(tags))
	for _, tag := range tags {
		commit := strings.ToLower(strings.TrimSpace(tag.Commit.SHA))
		if !commitPattern.MatchString(commit) || !versionTagPattern.MatchString(tag.Name) {
			continue
		}
		if _, exists := versions[commit]; !exists {
			versions[commit] = tag.Name
		}
	}
	return versions
}

func (s *Server) prebuiltImageAvailable(ctx context.Context, commit string) (bool, string, error) {
	if s.imageChecker != nil {
		return s.imageChecker(ctx, commit)
	}
	repository := strings.TrimSpace(s.updateImageRepository())
	repository = strings.TrimPrefix(repository, "https://")
	repository = strings.TrimPrefix(repository, "http://")
	registry, path := "docker.io", repository
	if strings.HasPrefix(path, "docker.io/") {
		path = strings.TrimPrefix(path, "docker.io/")
	} else if slash := strings.IndexByte(path, '/'); slash > 0 && strings.Contains(path[:slash], ".") {
		registry, path = path[:slash], path[slash+1:]
	}
	tag := imageTag(commit)
	var endpoint string
	if registry == "docker.io" {
		endpoint = "https://hub.docker.com/v2/repositories/" + path + "/tags/" + url.PathEscape(tag)
	} else {
		endpoint = "https://" + registry + "/v2/" + path + "/manifests/" + url.PathEscape(tag)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, "", err
	}
	request.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	request.Header.Set("User-Agent", "ecs-controller-image-check")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return false, "", err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return false, "", nil
	}
	if response.StatusCode != http.StatusOK {
		return false, "", fmt.Errorf("镜像仓库返回 HTTP %d", response.StatusCode)
	}
	if digest := response.Header.Get("Docker-Content-Digest"); digest != "" {
		return true, digest, nil
	}
	var tagInfo struct {
		Digest string `json:"digest"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&tagInfo); err != nil && registry != "docker.io" {
		return false, "", err
	}
	return true, tagInfo.Digest, nil
}

func (s *Server) updateStatus(w http.ResponseWriter) {
	currentCommit := strings.TrimSpace(app.Commit)
	status := map[string]any{
		"status":         "idle",
		"configured":     s.updateConfigured(),
		"current_commit": currentCommit,
	}
	if !s.updateConfigured() {
		status["message"] = "当前部署未启用 Docker 在线更新"
		s.json(w, http.StatusOK, status)
		return
	}
	path := filepath.Join(s.UpdateDir, "status.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var stored map[string]any
		if json.Unmarshal(raw, &stored) == nil {
			for key, value := range stored {
				status[key] = value
			}
			// The controller restarts before the updater writes its final status.
			// If this process is already running the requested commit, the update
			// has completed even when the status file is still in "restarting".
			targetCommit := strings.TrimSpace(stringValue(stored["target_commit"]))
			storedStatus := strings.TrimSpace(stringValue(stored["status"]))
			if commitPattern.MatchString(currentCommit) && strings.EqualFold(targetCommit, currentCommit) && (storedStatus == "queued" || storedStatus == "running") {
				status["status"] = "success"
				status["phase"] = "completed"
				status["message"] = "更新完成，当前已运行最新版本"
				status["progress"] = 100
				status["current_commit"] = currentCommit
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		status["message"] = "更新状态读取失败"
	}
	s.json(w, http.StatusOK, status)
}

func (s *Server) startUpdate(w http.ResponseWriter, r *http.Request, data map[string]any) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if !s.updateConfigured() {
		s.error(w, http.StatusServiceUnavailable, "当前部署未启用 Docker 在线更新，请使用 install.sh 更新")
		return
	}
	targetSHA := strings.ToLower(strings.TrimSpace(stringValue(data["target_commit"])))
	if !commitPattern.MatchString(targetSHA) {
		s.error(w, http.StatusBadRequest, "更新版本标识无效，请重新检查更新")
		return
	}
	currentCommit := strings.TrimSpace(app.Commit)
	if commitPattern.MatchString(currentCommit) && strings.EqualFold(currentCommit, targetSHA) {
		s.error(w, http.StatusConflict, "当前已经是目标版本")
		return
	}
	checkContext, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	imageAvailable, _, imageErr := s.prebuiltImageAvailable(checkContext, targetSHA)
	if imageErr != nil {
		s.error(w, http.StatusServiceUnavailable, "无法确认目标版本的预构建 Docker 镜像，请稍后重试")
		return
	}
	if !imageAvailable {
		s.error(w, http.StatusConflict, "目标版本的预构建 Docker 镜像尚未发布，请稍后重试")
		return
	}
	state := s.readUpdateState()
	if state == "queued" || state == "running" {
		s.error(w, http.StatusConflict, "已有更新任务正在执行")
		return
	}
	if _, err := os.Stat(filepath.Join(s.UpdateDir, "request.json")); err == nil {
		s.error(w, http.StatusConflict, "已有更新请求等待执行")
		return
	}
	if _, err := os.Stat(filepath.Join(s.UpdateDir, "request.processing.json")); err == nil {
		s.error(w, http.StatusConflict, "已有更新请求等待执行")
		return
	}
	if err := os.MkdirAll(s.UpdateDir, 0700); err != nil {
		s.error(w, http.StatusInternalServerError, "更新目录不可用")
		return
	}
	request := updateRequest{RequestID: randomToken(16), TargetSHA: targetSHA, RequestedAt: time.Now().Unix()}
	path := filepath.Join(s.UpdateDir, "request.json")
	temporary := path + ".tmp"
	raw, _ := json.Marshal(request)
	if err := os.WriteFile(temporary, raw, 0600); err != nil {
		s.error(w, http.StatusInternalServerError, "更新请求写入失败")
		return
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		s.error(w, http.StatusInternalServerError, "更新请求提交失败")
		return
	}
	s.json(w, http.StatusAccepted, map[string]any{"success": true, "request_id": request.RequestID, "status": "queued"})
}

func (s *Server) readUpdateState() string {
	if !s.updateConfigured() {
		return "idle"
	}
	raw, err := os.ReadFile(filepath.Join(s.UpdateDir, "status.json"))
	if err != nil {
		return "idle"
	}
	var state struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return "idle"
	}
	return state.Status
}

func shortCommit(commit string) string {
	commit = strings.TrimSpace(commit)
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}
